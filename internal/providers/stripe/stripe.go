// Package stripe is aether's payment gateway on Stripe Connect: Standard
// accounts, direct charges on the merchant's account, refunds, and Connect
// webhooks verified by signature.
package stripe

import (
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	stripe "github.com/stripe/stripe-go/v84"
	"github.com/stripe/stripe-go/v84/webhook"

	"aether/internal/commerce"
	"aether/internal/ports"
)

// Stripe rejects events signed longer ago than this.
const signatureToleranceSecs = 300

type Config struct {
	// APIURL is e.g. https://api.stripe.com.
	APIURL string
	// SecretKey is the platform secret key (sk_test_... in dev and staging).
	SecretKey string
	// PublishableKey is the platform publishable key, handed to storefronts.
	PublishableKey string
	// WebhookSecret is the signing secret of the Connect webhook endpoint.
	WebhookSecret string
}

type Gateway struct {
	client *stripe.Client
	cfg    Config
}

var _ ports.PaymentGateway = (*Gateway)(nil)

func New(httpClient *http.Client, cfg Config) *Gateway {
	backends := stripe.NewBackendsWithConfig(&stripe.BackendConfig{
		HTTPClient:        httpClient,
		URL:               stripe.String(cfg.APIURL),
		MaxNetworkRetries: stripe.Int64(0),
		LeveledLogger:     &stripe.LeveledLogger{Level: stripe.LevelNull},
	})
	return &Gateway{
		client: stripe.NewClient(cfg.SecretKey, stripe.WithBackends(backends)),
		cfg:    cfg,
	}
}

func (g *Gateway) Provider() string { return "stripe" }

// CreateAccount puts siteID in metadata so account.updated scopes to the site.
func (g *Gateway) CreateAccount(ctx context.Context, siteID uuid.UUID) (string, error) {
	params := &stripe.AccountCreateParams{Type: stripe.String("standard")}
	params.AddMetadata("site_id", siteID.String())
	params.SetIdempotencyKey("account:" + siteID.String())
	account, err := g.client.V1Accounts.Create(ctx, params)
	if err != nil {
		return "", classify(err)
	}
	return account.ID, nil
}

func (g *Gateway) OnboardingLink(ctx context.Context, account, returnURL, refreshURL string) (string, error) {
	link, err := g.client.V1AccountLinks.Create(ctx, &stripe.AccountLinkCreateParams{
		Account:    stripe.String(account),
		Type:       stripe.String("account_onboarding"),
		ReturnURL:  stripe.String(returnURL),
		RefreshURL: stripe.String(refreshURL),
	})
	if err != nil {
		return "", classify(err)
	}
	return link.URL, nil
}

func (g *Gateway) AccountStatus(ctx context.Context, account string) (ports.AccountStatus, error) {
	acct, err := g.client.V1Accounts.GetByID(ctx, account, nil)
	if err != nil {
		return ports.AccountStatus{}, classify(err)
	}
	return ports.AccountStatus{
		PaymentsEnabled:  acct.ChargesEnabled,
		DetailsSubmitted: acct.DetailsSubmitted,
	}, nil
}

type client struct {
	ClientSecret   string `json:"client_secret"`
	PublishableKey string `json:"publishable_key"`
	StripeAccount  string `json:"stripe_account"`
}

// StartPayment makes a direct charge on the merchant's account; the
// storefront confirms it with Stripe.js using Client.
func (g *Gateway) StartPayment(ctx context.Context, req ports.PaymentRequest) (ports.StartedPayment, error) {
	amount, ok := minorUnits(req.Amount, req.Currency)
	if !ok {
		return ports.StartedPayment{}, ports.Reject("%s %s is not a whole number of minor units", req.Amount, req.Currency)
	}
	params := &stripe.PaymentIntentCreateParams{
		Amount:   stripe.Int64(amount),
		Currency: stripe.String(strings.ToLower(req.Currency)),
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),
		},
	}
	params.AddMetadata("site_id", req.SiteID.String())
	params.AddMetadata("order_id", req.OrderID.String())
	params.SetStripeAccount(req.Account)
	params.SetIdempotencyKey(req.IdempotencyKey)
	intent, err := g.client.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return ports.StartedPayment{}, classify(err)
	}
	if intent.ClientSecret == "" {
		return ports.StartedPayment{}, ports.Unavailablef("stripe returned no client secret")
	}
	c, err := json.Marshal(client{
		ClientSecret:   intent.ClientSecret,
		PublishableKey: g.cfg.PublishableKey,
		StripeAccount:  req.Account,
	})
	if err != nil {
		return ports.StartedPayment{}, err
	}
	return ports.StartedPayment{
		Reference: intent.ID,
		Status:    paymentStatus(string(intent.Status)),
		Client:    c,
	}, nil
}

func (g *Gateway) CancelPayment(ctx context.Context, account, reference string) (ports.PaymentStatus, error) {
	params := &stripe.PaymentIntentCancelParams{}
	params.SetStripeAccount(account)
	intent, err := g.client.V1PaymentIntents.Cancel(ctx, reference, params)
	if err != nil {
		return "", classify(err)
	}
	return paymentStatus(string(intent.Status)), nil
}

func (g *Gateway) Refund(ctx context.Context, req ports.RefundRequest) (ports.RefundOutcome, error) {
	amount, ok := minorUnits(req.Amount, req.Currency)
	if !ok {
		return ports.RefundOutcome{}, ports.Reject("refund amount is not a whole number of minor units")
	}
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(req.PaymentReference),
		Amount:        stripe.Int64(amount),
	}
	params.AddMetadata("site_id", req.SiteID.String())
	params.AddMetadata("refund_id", req.RefundID.String())
	params.SetStripeAccount(req.Account)
	params.SetIdempotencyKey("refund:" + req.RefundID.String())
	refund, err := g.client.V1Refunds.Create(ctx, params)
	if err != nil {
		return ports.RefundOutcome{}, classify(err)
	}
	return ports.RefundOutcome{Reference: refund.ID, Status: refundStatus(string(refund.Status))}, nil
}

func (g *Gateway) ParseEvent(header http.Header, body []byte) (*ports.Event, error) {
	if !verifySignature(g.cfg.WebhookSecret, body, header.Get("Stripe-Signature"), time.Now().Unix()) {
		return nil, fmt.Errorf("%w: bad Stripe signature", ports.ErrInvalidEvent)
	}
	var e event
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, fmt.Errorf("%w: %v", ports.ErrInvalidEvent, err)
	}
	return readEvent(e), nil
}

// classify: network errors, 5xx and 429 are worth retrying; other 4xx are
// refusals.
func classify(err error) error {
	var se *stripe.Error
	if !errors.As(err, &se) {
		return ports.Unavailablef("stripe: %v", err)
	}
	if se.HTTPStatusCode >= 500 || se.HTTPStatusCode == http.StatusTooManyRequests {
		return ports.Unavailablef("stripe returned %d", se.HTTPStatusCode)
	}
	message := se.Msg
	if message == "" {
		message = "request refused"
	}
	return ports.Reject("Stripe: %s", message)
}

// minorUnits converts amount to Stripe's integer minor units; fractions are
// refused.
func minorUnits(amount decimal.Decimal, currency string) (int64, bool) {
	scaled := amount.Shift(commerce.MinorUnits(currency))
	if !scaled.IsInteger() || !scaled.BigInt().IsInt64() {
		return 0, false
	}
	return scaled.IntPart(), true
}

func paymentStatus(stripe string) ports.PaymentStatus {
	switch stripe {
	case "processing", "requires_capture":
		return ports.PaymentProcessing
	case "succeeded":
		return ports.PaymentSucceeded
	case "canceled":
		return ports.PaymentCancelled
	default:
		return ports.PaymentOpen
	}
}

func refundStatus(stripe string) ports.RefundStatus {
	switch stripe {
	case "succeeded":
		return ports.RefundSucceeded
	case "failed":
		return ports.RefundFailed
	case "canceled":
		return ports.RefundCancelled
	default:
		return ports.RefundPending
	}
}

type event struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Account string `json:"account"`
	Data    struct {
		Object struct {
			ID             string            `json:"id"`
			Status         string            `json:"status"`
			ChargesEnabled bool              `json:"charges_enabled"`
			Metadata       map[string]string `json:"metadata"`
		} `json:"object"`
	} `json:"data"`
}

// readEvent maps a verified Stripe event to aether's terms; nil means one
// aether does not act on (or not created by aether: no site in metadata).
func readEvent(e event) *ports.Event {
	object := e.Data.Object
	metadataUUID := func(key string) (uuid.UUID, bool) {
		id, err := uuid.Parse(object.Metadata[key])
		return id, err == nil
	}
	if strings.HasPrefix(e.Type, "payment_intent.") && object.ID == "" {
		return nil
	}
	out := ports.Event{ID: e.ID, Account: e.Account}
	switch e.Type {
	case "payment_intent.succeeded":
		orderID, ok := metadataUUID("order_id")
		if !ok {
			return nil
		}
		out.Type, out.OrderID, out.Reference = ports.PaymentSucceededEvent, orderID, object.ID
	case "payment_intent.payment_failed":
		out.Type, out.Reference = ports.PaymentFailedEvent, object.ID
	case "payment_intent.canceled":
		out.Type, out.Reference = ports.PaymentCancelledEvent, object.ID
	case "refund.created", "refund.updated":
		status := refundStatus(object.Status)
		refundID, ok := metadataUUID("refund_id")
		if status == ports.RefundPending || !ok {
			return nil
		}
		out.Type, out.RefundID, out.RefundStatus = ports.RefundSettled, refundID, status
	case "account.updated":
		out.Type, out.PaymentsEnabled = ports.AccountUpdated, object.ChargesEnabled
	default:
		return nil
	}
	siteID, ok := metadataUUID("site_id")
	if e.ID == "" || !ok {
		return nil
	}
	out.SiteID = siteID
	return &out
}

// verifySignature checks a Stripe-Signature header,
// `t=<unix time>,v1=<hex hmac-sha256 of "t.payload">`; any matching v1 passes.
func verifySignature(secret string, payload []byte, header string, now int64) bool {
	var (
		timestamp    int64
		hasTimestamp bool
		signatures   []string
	)
	for part := range strings.SplitSeq(header, ",") {
		key, value, _ := strings.Cut(part, "=")
		switch key {
		case "t":
			t, err := strconv.ParseInt(value, 10, 64)
			timestamp, hasTimestamp = t, err == nil
		case "v1":
			signatures = append(signatures, value)
		}
	}
	if !hasTimestamp || max(now-timestamp, timestamp-now) > signatureToleranceSecs {
		return false
	}
	expected := webhook.ComputeSignature(time.Unix(timestamp, 0), payload, secret)
	for _, sig := range signatures {
		if got, err := hex.DecodeString(sig); err == nil && hmac.Equal(got, expected) {
			return true
		}
	}
	return false
}
