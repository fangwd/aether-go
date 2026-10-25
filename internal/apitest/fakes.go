package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"aether/internal/ports"
)

// FakeImages hands out numbered upload slots.
type FakeImages struct{ uploads atomic.Int64 }

func (*FakeImages) Provider() string { return "fake_images" }

func (f *FakeImages) BeginUpload(context.Context, uuid.UUID) (ports.Upload, error) {
	n := f.uploads.Add(1)
	return ports.Upload{Key: fmt.Sprintf("img-%d", n), UploadURL: fmt.Sprintf("https://upload.example/%d", n)}, nil
}

func (*FakeImages) PublicURL(key string) string { return "https://images.example/" + key }

// RecordingMailer keeps every message; domains verify at once.
type RecordingMailer struct {
	ports.LogMailer
	mu   sync.Mutex
	sent []ports.Email
}

func (m *RecordingMailer) Send(_ context.Context, e ports.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, e)
	return nil
}

func (m *RecordingMailer) Sent() []ports.Email {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ports.Email(nil), m.sent...)
}

// FailingMailer fails every delivery with Err.
type FailingMailer struct {
	ports.LogMailer
	Err error
}

func (m FailingMailer) Send(context.Context, ports.Email) error { return m.Err }

// FakeAccount is the merchant account FakeGateway creates.
const FakeAccount = "acct_merchant"

// FakeSignature is the only header value FakeGateway accepts on events.
const FakeSignature = "fake-valid"

// FakeGateway is a payment provider whose answers tests script; it records
// every call.
type FakeGateway struct {
	mu              sync.Mutex
	calls           []string
	PaymentsEnabled bool
	payments        int
	// CancelResults are the scripted answers to CancelPayment; default:
	// cancelled.
	CancelResults []func() (ports.PaymentStatus, error)
	// RefundStatuses are scripted refund outcomes; default: succeeded.
	RefundStatuses []ports.RefundStatus
	// RefundIDs records the refund id of every refund requested.
	RefundIDs []uuid.UUID
	// StartError, when set, fails the next StartPayment.
	StartError error
}

func (g *FakeGateway) Calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *FakeGateway) record(call string) { g.calls = append(g.calls, call) }

func (*FakeGateway) Provider() string { return "fake" }

func (g *FakeGateway) CreateAccount(context.Context, uuid.UUID) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("create_account")
	return FakeAccount, nil
}

func (*FakeGateway) OnboardingLink(_ context.Context, account, _, _ string) (string, error) {
	return "https://pay.example/onboard/" + account, nil
}

func (g *FakeGateway) AccountStatus(context.Context, string) (ports.AccountStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return ports.AccountStatus{PaymentsEnabled: g.PaymentsEnabled, DetailsSubmitted: true}, nil
}

func (g *FakeGateway) StartPayment(_ context.Context, req ports.PaymentRequest) (ports.StartedPayment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record(fmt.Sprintf("start_payment %s %s %s", req.Amount.StringFixed(2), req.Currency, req.Account))
	if err := g.StartError; err != nil {
		g.StartError = nil
		return ports.StartedPayment{}, err
	}
	g.payments++
	client, _ := json.Marshal(map[string]string{"secret": fmt.Sprintf("secret_%d", g.payments)})
	return ports.StartedPayment{Reference: fmt.Sprintf("pay_%d", g.payments), Status: ports.PaymentOpen, Client: client}, nil
}

func (g *FakeGateway) CancelPayment(_ context.Context, _, reference string) (ports.PaymentStatus, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record("cancel_payment " + reference)
	if len(g.CancelResults) > 0 {
		next := g.CancelResults[0]
		g.CancelResults = g.CancelResults[1:]
		return next()
	}
	return ports.PaymentCancelled, nil
}

func (g *FakeGateway) Refund(_ context.Context, req ports.RefundRequest) (ports.RefundOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.record(fmt.Sprintf("refund %s of %s", req.Amount.StringFixed(2), req.PaymentReference))
	g.RefundIDs = append(g.RefundIDs, req.RefundID)
	status := ports.RefundSucceeded
	if len(g.RefundStatuses) > 0 {
		status, g.RefundStatuses = g.RefundStatuses[0], g.RefundStatuses[1:]
	}
	return ports.RefundOutcome{Reference: "ref_" + req.RefundID.String(), Status: status}, nil
}

// ParseEvent reads JSON events {id, account, site_id, kind: {type, ...}}
// sent with the header fake-signature: fake-valid.
func (*FakeGateway) ParseEvent(header http.Header, body []byte) (*ports.Event, error) {
	if header.Get("fake-signature") != FakeSignature {
		return nil, fmt.Errorf("%w: bad signature", ports.ErrInvalidEvent)
	}
	var v struct {
		ID      string    `json:"id"`
		Account string    `json:"account"`
		SiteID  uuid.UUID `json:"site_id"`
		Kind    struct {
			Type            string    `json:"type"`
			PaymentsEnabled bool      `json:"payments_enabled"`
			OrderID         uuid.UUID `json:"order_id"`
			Reference       string    `json:"reference"`
			RefundID        uuid.UUID `json:"refund_id"`
			Status          string    `json:"status"`
		} `json:"kind"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("%w: %v", ports.ErrInvalidEvent, err)
	}
	ev := &ports.Event{ID: v.ID, Account: v.Account, SiteID: v.SiteID, OrderID: v.Kind.OrderID,
		Reference: v.Kind.Reference, RefundID: v.Kind.RefundID, PaymentsEnabled: v.Kind.PaymentsEnabled}
	switch v.Kind.Type {
	case "account_updated":
		ev.Type = ports.AccountUpdated
	case "payment_succeeded":
		ev.Type = ports.PaymentSucceededEvent
	case "payment_failed":
		ev.Type = ports.PaymentFailedEvent
	case "refund_settled":
		ev.Type = ports.RefundSettled
		ev.RefundStatus = ports.RefundFailed
		if v.Kind.Status == "succeeded" {
			ev.RefundStatus = ports.RefundSucceeded
		}
	default:
		return nil, nil
	}
	return ev, nil
}
