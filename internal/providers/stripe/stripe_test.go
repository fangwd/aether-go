package stripe

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stripe/stripe-go/v84/webhook"

	"aether/internal/ports"
)

const (
	secret  = "whsec_test"
	account = "acct_merchant"
)

// sign signs a payload the way Stripe does.
func sign(secret string, payload []byte, timestamp int64) string {
	sig := webhook.ComputeSignature(time.Unix(timestamp, 0), payload, secret)
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(sig))
}

func TestAmountsBecomeWholeMinorUnits(t *testing.T) {
	for _, c := range []struct {
		amount, currency string
		want             int64
		ok               bool
	}{{"50.88", "USD", 5088, true}, {"1500", "JPY", 1500, true}, {"1.005", "USD", 0, false}} {
		got, ok := minorUnits(decimal.RequireFromString(c.amount), c.currency)
		if got != c.want || ok != c.ok {
			t.Errorf("minorUnits(%s %s) = %d, %v; want %d, %v", c.amount, c.currency, got, ok, c.want, c.ok)
		}
	}
}

func TestSignaturesVerifyWithinToleranceOnly(t *testing.T) {
	payload := []byte(`{"id":"evt_1"}`)
	header := sign("whsec_test", payload, 1_000)
	if !verifySignature("whsec_test", payload, header, 1_100) {
		t.Error("valid signature refused")
	}
	if verifySignature("whsec_test", payload, header, 1_000+301) {
		t.Error("expired signature accepted")
	}
	if verifySignature("whsec_other", payload, header, 1_100) {
		t.Error("wrong secret accepted")
	}
	if verifySignature("whsec_test", []byte(`{"id":"evt_2"}`), header, 1_100) {
		t.Error("altered payload accepted")
	}
	if verifySignature("whsec_test", payload, "v1=00", 1_100) {
		t.Error("header without timestamp accepted")
	}
}

// route is one expected Stripe call: the handler checks the request and
// answers with status and body.
type route struct {
	method, path string
	headers      map[string]string
	form         map[string]string
	status       int
	body         any
	calls        int
}

func server(t *testing.T, routes ...*route) *Gateway {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, rt := range routes {
			if r.Method != rt.method || r.URL.Path != rt.path || !matches(r, string(body), rt) {
				continue
			}
			rt.calls++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rt.status)
			_ = json.NewEncoder(w).Encode(rt.body)
			return
		}
		t.Errorf("unexpected request %s %s %s", r.Method, r.URL.Path, body)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.Client(), Config{
		APIURL:         srv.URL,
		SecretKey:      "sk_test_platform",
		PublishableKey: "pk_test_platform",
		WebhookSecret:  secret,
	})
}

func matches(r *http.Request, body string, rt *route) bool {
	for k, v := range rt.headers {
		if r.Header.Get(k) != v {
			return false
		}
	}
	form, err := url.ParseQuery(body)
	if err != nil {
		return false
	}
	for k, v := range rt.form {
		if form.Get(k) != v {
			return false
		}
	}
	return true
}

func TestAccountsAreStandardAndReportCharges(t *testing.T) {
	siteID := uuid.Must(uuid.NewV7())
	create := &route{
		method: "POST", path: "/v1/accounts",
		headers: map[string]string{
			"Authorization":   "Bearer sk_test_platform",
			"Idempotency-Key": "account:" + siteID.String(),
		},
		form:   map[string]string{"type": "standard"},
		status: 200, body: map[string]any{"id": account, "charges_enabled": false},
	}
	get := &route{
		method: "GET", path: "/v1/accounts/" + account,
		status: 200, body: map[string]any{"id": account, "charges_enabled": true, "details_submitted": true},
	}
	stripe := server(t, create, get)
	ctx := context.Background()

	id, err := stripe.CreateAccount(ctx, siteID)
	if err != nil || id != account {
		t.Fatalf("CreateAccount = %q, %v", id, err)
	}
	status, err := stripe.AccountStatus(ctx, account)
	if err != nil || !status.PaymentsEnabled || !status.DetailsSubmitted {
		t.Fatalf("AccountStatus = %+v, %v", status, err)
	}
	if create.calls != 1 {
		t.Errorf("account created %d times, want 1", create.calls)
	}
}

func TestPaymentsAreDirectChargesInMinorUnits(t *testing.T) {
	create := &route{
		method: "POST", path: "/v1/payment_intents",
		headers: map[string]string{"Stripe-Account": account, "Idempotency-Key": "pay-1"},
		form:    map[string]string{"amount": "2100", "currency": "usd"},
		status:  200, body: map[string]any{"id": "pi_1", "client_secret": "pi_1_secret", "status": "requires_payment_method"},
	}

	started, err := server(t, create).StartPayment(context.Background(), ports.PaymentRequest{
		Account:        account,
		Amount:         decimal.New(2100, -2),
		Currency:       "USD",
		SiteID:         uuid.Must(uuid.NewV7()),
		OrderID:        uuid.Must(uuid.NewV7()),
		IdempotencyKey: "pay-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	if started.Reference != "pi_1" || started.Status != ports.PaymentOpen {
		t.Errorf("started = %q, %q", started.Reference, started.Status)
	}
	var got, want map[string]any
	_ = json.Unmarshal(started.Client, &got)
	_ = json.Unmarshal([]byte(`{"client_secret":"pi_1_secret","publishable_key":"pk_test_platform","stripe_account":"`+account+`"}`), &want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("client = %s", started.Client)
	}
	if create.calls != 1 {
		t.Errorf("payment created %d times, want 1", create.calls)
	}
}

func TestCancellingASucceededPaymentIsRejected(t *testing.T) {
	stripe := server(t, &route{
		method: "POST", path: "/v1/payment_intents/pi_1/cancel",
		status: 400, body: map[string]any{"error": map[string]any{"message": "already succeeded"}},
	})

	_, err := stripe.CancelPayment(context.Background(), account, "pi_1")

	var pe *ports.ProviderError
	if !errors.As(err, &pe) || pe.Kind != ports.Rejected || !strings.Contains(pe.Message, "already succeeded") {
		t.Fatalf("err = %v, want Rejected", err)
	}
}

func TestOutagesAreUnavailableNotRejections(t *testing.T) {
	stripe := server(t, &route{
		method: "POST", path: "/v1/payment_intents/pi_1/cancel",
		status: 503, body: map[string]any{},
	})

	_, err := stripe.CancelPayment(context.Background(), account, "pi_1")

	var pe *ports.ProviderError
	if !errors.As(err, &pe) || pe.Kind != ports.Unavailable {
		t.Fatalf("err = %v, want Unavailable", err)
	}
}

func TestRefundsCarryTheRefundID(t *testing.T) {
	refundID := uuid.Must(uuid.NewV7())
	create := &route{
		method: "POST", path: "/v1/refunds",
		headers: map[string]string{"Stripe-Account": account, "Idempotency-Key": "refund:" + refundID.String()},
		form: map[string]string{
			"payment_intent": "pi_1", "amount": "1000",
			"metadata[refund_id]": refundID.String(),
		},
		status: 200, body: map[string]any{"id": "re_1", "status": "pending"},
	}

	outcome, err := server(t, create).Refund(context.Background(), ports.RefundRequest{
		Account:          account,
		PaymentReference: "pi_1",
		Amount:           decimal.New(1000, -2),
		Currency:         "USD",
		SiteID:           uuid.Must(uuid.NewV7()),
		RefundID:         refundID,
	})
	if err != nil {
		t.Fatal(err)
	}

	if outcome.Reference != "re_1" || outcome.Status != ports.RefundPending {
		t.Errorf("outcome = %+v", outcome)
	}
	if create.calls != 1 {
		t.Errorf("refund created %d times, want 1", create.calls)
	}
}

func signed(t *testing.T, event any) (http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{}
	header.Set("Stripe-Signature", sign(secret, body, time.Now().Unix()))
	return header, body
}

func TestSignedEventsAreReadAndForgedOnesRefused(t *testing.T) {
	stripe := server(t)
	siteID, orderID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	paid := map[string]any{"id": "evt_1", "type": "payment_intent.succeeded", "account": account,
		"data": map[string]any{"object": map[string]any{"id": "pi_1", "metadata": map[string]any{"site_id": siteID, "order_id": orderID}}}}

	header, body := signed(t, paid)
	event, err := stripe.ParseEvent(header, body)
	if err != nil || event == nil {
		t.Fatalf("ParseEvent = %v, %v", event, err)
	}
	if event.ID != "evt_1" || event.Account != account || event.SiteID != siteID {
		t.Errorf("event = %+v", event)
	}
	if event.Type != ports.PaymentSucceededEvent || event.OrderID != orderID || event.Reference != "pi_1" {
		t.Errorf("event = %+v", event)
	}

	forged := http.Header{}
	forged.Set("Stripe-Signature", "t=1,v1=00")
	if _, err := stripe.ParseEvent(forged, body); !errors.Is(err, ports.ErrInvalidEvent) {
		t.Errorf("forged event: err = %v", err)
	}

	pending := map[string]any{"id": "evt_2", "type": "refund.updated",
		"data": map[string]any{"object": map[string]any{"id": "re_1", "status": "pending", "metadata": map[string]any{"site_id": siteID, "refund_id": uuid.Must(uuid.NewV7())}}}}
	header, body = signed(t, pending)
	if event, err := stripe.ParseEvent(header, body); err != nil || event != nil {
		t.Errorf("only final refund states settle: %+v, %v", event, err)
	}

	foreign := map[string]any{"id": "evt_3", "type": "payment_intent.succeeded",
		"data": map[string]any{"object": map[string]any{"id": "pi_9", "metadata": map[string]any{}}}}
	header, body = signed(t, foreign)
	if event, err := stripe.ParseEvent(header, body); err != nil || event != nil {
		t.Errorf("not created by aether: %+v, %v", event, err)
	}
}
