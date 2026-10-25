package ports

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type AccountStatus struct {
	PaymentsEnabled  bool
	DetailsSubmitted bool
}

// PaymentStatus is where a payment stands; stored as payment.status.
type PaymentStatus string

const (
	// PaymentOpen: waiting for the customer to pay.
	PaymentOpen       PaymentStatus = "open"
	PaymentProcessing PaymentStatus = "processing"
	PaymentSucceeded  PaymentStatus = "succeeded"
	PaymentFailed     PaymentStatus = "failed"
	PaymentCancelled  PaymentStatus = "cancelled"
)

// RefundStatus is stored as refund.status.
type RefundStatus string

const (
	RefundPending   RefundStatus = "pending"
	RefundSucceeded RefundStatus = "succeeded"
	RefundFailed    RefundStatus = "failed"
	RefundCancelled RefundStatus = "cancelled"
)

type PaymentRequest struct {
	// Account is the merchant's account at the provider.
	Account  string
	Amount   decimal.Decimal
	Currency string
	SiteID   uuid.UUID
	OrderID  uuid.UUID
	// IdempotencyKey: repeating a request with the same key must not
	// charge twice.
	IdempotencyKey string
}

type StartedPayment struct {
	// Reference is the provider's id for this payment.
	Reference string
	Status    PaymentStatus
	// Client is what the storefront's payment SDK needs to collect the
	// payment; passed through untouched.
	Client json.RawMessage
}

type RefundRequest struct {
	Account string
	// PaymentReference is the provider's id of the payment being refunded.
	PaymentReference string
	Amount           decimal.Decimal
	Currency         string
	SiteID           uuid.UUID
	RefundID         uuid.UUID
}

type RefundOutcome struct {
	Reference string
	Status    RefundStatus
}

type EventType int

const (
	AccountUpdated EventType = iota + 1
	PaymentSucceededEvent
	PaymentFailedEvent
	PaymentCancelledEvent
	RefundSettled
)

// Event is a provider notification, already authenticated. Which fields
// are set depends on Type.
type Event struct {
	// ID is unique per event, for applying each once.
	ID string
	// Account is the merchant account the event concerns, if any.
	Account string
	SiteID  uuid.UUID
	Type    EventType

	PaymentsEnabled bool         // AccountUpdated
	OrderID         uuid.UUID    // PaymentSucceededEvent
	Reference       string       // payment events
	RefundID        uuid.UUID    // RefundSettled
	RefundStatus    RefundStatus // RefundSettled
}

// ErrInvalidEvent: the webhook could not be authenticated or read.
var ErrInvalidEvent = errors.New("invalid payment event")

// PaymentGateway takes payments through a payment provider on each
// merchant's own account. Providers translate their API and webhooks into
// these terms.
type PaymentGateway interface {
	// Provider names the provider in routes and stored rows, e.g. in
	// /v1/webhooks/payments/{provider}.
	Provider() string
	// CreateAccount creates the merchant's account for a site.
	CreateAccount(ctx context.Context, siteID uuid.UUID) (string, error)
	// OnboardingLink is a one-time link to the provider's hosted onboarding.
	OnboardingLink(ctx context.Context, account, returnURL, refreshURL string) (string, error)
	AccountStatus(ctx context.Context, account string) (AccountStatus, error)
	StartPayment(ctx context.Context, req PaymentRequest) (StartedPayment, error)
	// CancelPayment is Rejected when the payment can no longer be
	// cancelled (it went through).
	CancelPayment(ctx context.Context, account, reference string) (PaymentStatus, error)
	Refund(ctx context.Context, req RefundRequest) (RefundOutcome, error)
	// ParseEvent authenticates and reads a webhook. A nil event means one
	// aether does not act on; errors wrap ErrInvalidEvent.
	ParseEvent(header http.Header, body []byte) (*Event, error)
}
