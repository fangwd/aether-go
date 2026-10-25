package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
	"aether/internal/ports"
)

// openStatuses are payments still waiting on the customer or the provider.
var openStatuses = []string{string(ports.PaymentOpen), string(ports.PaymentProcessing)}

func (s *Server) gateway() (ports.PaymentGateway, error) {
	if s.Payments == nil {
		return nil, errUnavailable
	}
	return s.Payments, nil
}

// AccountStatus is the site's payment account, refreshed from the provider.
func (s *Server) AccountStatus(ctx context.Context, req oapi.AccountStatusRequestObject) (oapi.AccountStatusResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	gw, err := s.gateway()
	if err != nil {
		return nil, err
	}
	out := oapi.PaymentAccount{Provider: ptr(gw.Provider())}
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil || site.PaymentAccountID == nil {
			return err
		}
		status, err := gw.AccountStatus(ctx, *site.PaymentAccountID)
		if err != nil {
			return err
		}
		out.AccountId, out.PaymentsEnabled, out.DetailsSubmitted = site.PaymentAccountID, status.PaymentsEnabled, status.DetailsSubmitted
		return q.SetPaymentsEnabled(ctx, status.PaymentsEnabled)
	})
	if err != nil {
		return nil, err
	}
	return oapi.AccountStatus200JSONResponse(out), nil
}

// StartOnboarding connects the site to the payment provider (owner only)
// and returns a link to the provider's onboarding. Call again to resume
// unfinished onboarding.
func (s *Server) StartOnboarding(ctx context.Context, req oapi.StartOnboardingRequestObject) (oapi.StartOnboardingResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	gw, err := s.gateway()
	if err != nil {
		return nil, err
	}
	var url string
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		account := site.PaymentAccountID
		if account == nil {
			id, err := gw.CreateAccount(ctx, req.SiteId)
			if err != nil {
				return err
			}
			if err := q.SetPaymentAccount(ctx, &id); err != nil {
				return err
			}
			account = &id
		}
		url, err = gw.OnboardingLink(ctx, *account, req.Body.ReturnUrl, req.Body.RefreshUrl)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.StartOnboarding200JSONResponse{Url: url}, nil
}

// PayOrder starts (or resumes) paying an order. The order becomes paid
// when the provider confirms the payment.
func (s *Server) PayOrder(ctx context.Context, req oapi.PayOrderRequestObject) (oapi.PayOrderResponseObject, error) {
	access, err := s.orderAccess(ctx, req.SiteId, req.Params.XOrderToken)
	if err != nil {
		return nil, err
	}
	gw, err := s.gateway()
	if err != nil {
		return nil, err
	}
	var out oapi.Payment
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		if err := checkOrderAccess(ctx, q, access, req.OrderId); err != nil {
			return err
		}
		order, err := q.OrderToPay(ctx, req.OrderId)
		if err != nil {
			return err
		}
		if order.FulfilmentStatus == "cancelled" {
			return conflict("the order is cancelled")
		}
		if order.PaymentStatus != "pending" {
			return conflict("the order is already %s", order.PaymentStatus)
		}
		if site.PaymentAccountID == nil || !site.PaymentsEnabled {
			return conflict("this shop does not take online payments yet")
		}
		out = oapi.Payment{Provider: gw.Provider(), Amount: money(order.GrandTotal, order.Currency), Currency: order.Currency}
		var client json.RawMessage
		open, err := q.OpenPayment(ctx, db.OpenPaymentParams{OrderID: req.OrderId, Provider: gw.Provider(), Amount: order.GrandTotal})
		switch {
		case err == nil && open.ProviderReference != nil && open.Client != nil:
			out.PaymentReference, client = *open.ProviderReference, open.Client
		case err == nil || errors.Is(err, pgx.ErrNoRows):
			attempt, err := q.PaymentAttempts(ctx, req.OrderId)
			if err != nil {
				return err
			}
			started, err := gw.StartPayment(ctx, ports.PaymentRequest{
				Account: *site.PaymentAccountID, Amount: order.GrandTotal, Currency: order.Currency, SiteID: req.SiteId,
				OrderID: req.OrderId, IdempotencyKey: fmt.Sprintf("pay:%s:%d", req.OrderId, attempt),
			})
			if err != nil {
				return err
			}
			if err := q.InsertPayment(ctx, db.InsertPaymentParams{
				SiteID: req.SiteId, ID: newID(), OrderID: req.OrderId, Currency: order.Currency, Provider: gw.Provider(),
				ProviderReference: &started.Reference, Amount: order.GrandTotal, Status: string(started.Status), Client: started.Client,
			}); err != nil {
				return err
			}
			out.PaymentReference, client = started.Reference, started.Client
		default:
			return err
		}
		return json.Unmarshal(client, &out.Client)
	})
	if err != nil {
		return nil, err
	}
	return oapi.PayOrder200JSONResponse(out), nil
}

// RefundOrder refunds part or all of a paid order through the payment
// provider; refunded units can go back into stock.
func (s *Server) RefundOrder(ctx context.Context, req oapi.RefundOrderRequestObject) (oapi.RefundOrderResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	gw, err := s.gateway()
	if err != nil {
		return nil, err
	}
	refundAmount, err := amountOr("amount", &req.Body.Amount, decimal.Zero)
	if err != nil {
		return nil, err
	}
	items := deref(req.Body.Items, nil)
	var order oapi.Order
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		o, err := q.OrderToRefund(ctx, req.OrderId)
		if err != nil {
			return err
		}
		if o.PaymentStatus != "paid" && o.PaymentStatus != "partially_refunded" {
			return conflict("only paid orders can be refunded")
		}
		refundable := o.GrandTotal.Sub(o.RefundedTotal)
		if !refundAmount.IsPositive() || refundAmount.GreaterThan(refundable) {
			return unprocessable("amount must be above 0 and at most %s", commerce.Format(refundable, o.Currency))
		}
		if err := checkAmount("amount", refundAmount, o.Currency); err != nil {
			return err
		}
		for _, item := range items {
			room, err := q.RefundableQuantity(ctx, db.RefundableQuantityParams{ID: item.OrderItemId, OrderID: req.OrderId})
			if errors.Is(err, pgx.ErrNoRows) {
				return unprocessable("an item is not part of this order")
			}
			if err != nil {
				return err
			}
			if item.Quantity < 1 || item.Quantity > room {
				return unprocessable("that would refund more units than were bought")
			}
		}
		paid, err := q.SucceededPayment(ctx, req.OrderId)
		if errors.Is(err, pgx.ErrNoRows) || site.PaymentAccountID == nil || (err == nil && paid.ProviderReference == nil) {
			return conflict("no successful payment to refund")
		}
		if err != nil {
			return err
		}
		refundID := newID()
		outcome, err := gw.Refund(ctx, ports.RefundRequest{Account: *site.PaymentAccountID, PaymentReference: *paid.ProviderReference,
			Amount: refundAmount, Currency: o.Currency, SiteID: req.SiteId, RefundID: refundID})
		if err != nil {
			return err
		}
		itemsJSON, _ := json.Marshal(nonNil(items))
		if err := q.InsertRefund(ctx, db.InsertRefundParams{
			SiteID: req.SiteId, ID: refundID, OrderID: req.OrderId, Currency: o.Currency, PaymentID: &paid.ID,
			Provider: gw.Provider(), ProviderReference: &outcome.Reference, Amount: refundAmount, Reason: req.Body.Reason,
			Items: itemsJSON, Subject: staff.Subject,
		}); err != nil {
			return err
		}
		if outcome.Status != ports.RefundPending {
			if err := settleRefund(ctx, q, req.SiteId, refundID, outcome.Status); err != nil {
				return err
			}
		}
		order, err = loadOrder(ctx, q, req.OrderId, adminOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.RefundOrder200JSONResponse(order), nil
}

// settleRefund applies a refund's final status once: a succeeded refund
// raises the order's refunded total, marks units refunded and restocks them.
func settleRefund(ctx context.Context, q *db.Queries, siteID, refundID uuid.UUID, status ports.RefundStatus) error {
	refund, err := q.SettleRefund(ctx, db.SettleRefundParams{ID: refundID, Status: string(status)})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != ports.RefundSucceeded) {
		return nil
	}
	if err != nil {
		return err
	}
	from, err := q.OrderPaymentStatusForUpdate(ctx, refund.OrderID)
	if err != nil {
		return err
	}
	to, err := q.AddRefunded(ctx, db.AddRefundedParams{ID: refund.OrderID, Amount: refund.Amount})
	if err != nil {
		return err
	}
	var meta struct {
		Items   []oapi.RefundItem `json:"items"`
		Subject *string           `json:"subject"`
	}
	_ = json.Unmarshal(refund.Metadata, &meta)
	for _, item := range meta.Items {
		variant, err := q.MarkItemRefunded(ctx, db.MarkItemRefundedParams{ID: item.OrderItemId, RefundedQuantity: item.Quantity})
		if err != nil {
			return err
		}
		if variant != nil {
			if err := restock(ctx, q, siteID, *variant, int64(item.Quantity), refundID); err != nil {
				return err
			}
		}
	}
	reason := "refunded " + refund.Amount.String()
	if err := recordStatus(ctx, q, siteID, refund.OrderID, transition{kind: "payment", from: &from, to: to, reason: &reason, actor: meta.Subject}); err != nil {
		return err
	}
	return emit(ctx, q, siteID, "order.refunded", refund.OrderID, map[string]any{"order_id": refund.OrderID, "refund_id": refundID})
}

// markPaid marks an order paid and converts its held stock into a sale.
func markPaid(ctx context.Context, q *db.Queries, siteID uuid.UUID, provider string, orderID uuid.UUID, reference string) error {
	if _, err := q.MarkPaymentSucceeded(ctx, db.MarkPaymentSucceededParams{Provider: provider, ProviderReference: &reference}); errors.Is(err, pgx.ErrNoRows) {
		slog.Warn("payment event for an unknown payment", "order_id", orderID, "reference", reference)
		return nil
	} else if err != nil {
		return err
	}
	from, err := q.OrderPaymentStatusForUpdate(ctx, orderID)
	if err != nil || from != "pending" {
		return err
	}
	if err := q.MarkOrderPaid(ctx, orderID); err != nil {
		return err
	}
	if err := convertStock(ctx, q, siteID, orderID); err != nil {
		return err
	}
	if err := recordStatus(ctx, q, siteID, orderID, transition{kind: "payment", from: &from, to: "paid"}); err != nil {
		return err
	}
	return emit(ctx, q, siteID, "order.paid", orderID, map[string]any{"order_id": orderID})
}

// paymentWebhook is the payment provider's webhook. The provider
// authenticates and reads the event; aether scopes it to its site, checks
// the account, and applies it once per event id.
func (s *Server) paymentWebhook(w http.ResponseWriter, r *http.Request) {
	if err := s.applyPaymentEvent(r); err != nil {
		writeProblem(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) applyPaymentEvent(r *http.Request) error {
	ctx := r.Context()
	gw, err := s.gateway()
	if err != nil {
		return err
	}
	provider := r.PathValue("provider")
	if provider != gw.Provider() {
		return errNotFound
	}
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return badRequest("unreadable body")
	}
	event, err := gw.ParseEvent(r.Header, body)
	if err != nil {
		return badRequest("%s", err)
	}
	if event == nil {
		return nil
	}
	return db.InSite(ctx, s.Pool, event.SiteID, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		if deref(site.PaymentAccountID, "") != event.Account {
			slog.Warn("payment event for another account", "site_id", event.SiteID, "event_id", event.ID)
			return nil
		}
		key := provider + ":" + event.ID
		if replay, err := idempotencyClaim(ctx, q, event.SiteID, key, "payment-webhook"); err != nil || replay != nil {
			return err
		}
		switch event.Type {
		case ports.PaymentSucceededEvent:
			err = markPaid(ctx, q, event.SiteID, provider, event.OrderID, event.Reference)
		case ports.PaymentFailedEvent:
			err = q.ClosePayment(ctx, db.ClosePaymentParams{Provider: provider, ProviderReference: &event.Reference, Status: string(ports.PaymentFailed)})
		case ports.PaymentCancelledEvent:
			err = q.ClosePayment(ctx, db.ClosePaymentParams{Provider: provider, ProviderReference: &event.Reference, Status: string(ports.PaymentCancelled)})
		case ports.RefundSettled:
			err = settleRefund(ctx, q, event.SiteID, event.RefundID, event.RefundStatus)
		case ports.AccountUpdated:
			err = q.SetPaymentsEnabled(ctx, event.PaymentsEnabled)
		}
		if err != nil {
			return err
		}
		return idempotencyComplete(ctx, q, key, http.StatusOK, map[string]any{})
	})
}

// cancelOpenPayments cancels an order's open payments so a cancelled order
// cannot be paid. It fails with 409 unless the provider confirms each
// cancellation (a payment that went through, or is still processing,
// blocks it).
func cancelOpenPayments(ctx context.Context, q *db.Queries, payments ports.PaymentGateway, orderID uuid.UUID) error {
	open, err := q.OpenPayments(ctx, db.OpenPaymentsParams{OrderID: orderID, Statuses: openStatuses})
	if err != nil || len(open) == 0 {
		return err
	}
	if payments == nil {
		return errUnavailable
	}
	for _, p := range open {
		if p.Provider != payments.Provider() {
			return errUnavailable
		}
	}
	site, err := loadSite(ctx, q)
	if err != nil {
		return err
	}
	if site.PaymentAccountID == nil {
		return errInternal
	}
	for _, p := range open {
		status, err := payments.CancelPayment(ctx, *site.PaymentAccountID, deref(p.ProviderReference, ""))
		var provErr *ports.ProviderError
		if errors.As(err, &provErr) && provErr.Kind == ports.Rejected {
			return conflict("a payment for this order already went through; refund it instead")
		}
		if err != nil {
			return err
		}
		if status != ports.PaymentCancelled {
			return conflict("a payment for this order is still in progress; try again shortly")
		}
		if err := q.SetPaymentStatus(ctx, db.SetPaymentStatusParams{ID: p.ID, Status: string(ports.PaymentCancelled)}); err != nil {
			return err
		}
	}
	return nil
}
