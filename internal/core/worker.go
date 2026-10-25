package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"aether/internal/db"
	"aether/internal/ports"
)

const (
	maxAttempts = 10
	// unpaidMinutes: orders on sites that take online payments are
	// cancelled if unpaid this long.
	unpaidMinutes        = 30
	senderCheckMinutes   = 5
	outboxRetentionDays  = 7
	eventInterval        = 2 * time.Second
	housekeepingInterval = time.Minute
)

// Worker does the background work: delivering queued events (shop emails),
// cancelling unpaid orders, expiring carts and sessions, and checking
// sending domains.
//
// It connects as aether_worker (BYPASSRLS) only to find due work across
// sites. Each item is then handled in a transaction that switches to
// aether_app and sets the item's site, so RLS guards the work itself.
type Worker struct {
	Pool     *pgxpool.Pool
	Mailer   ports.Mailer
	Payments ports.PaymentGateway
	// FallbackFrom is the provider's verified sender address, used until a site
	// verifies its own domain. Empty: such sites send nothing.
	FallbackFrom string
}

// Run works until ctx ends: events every 2 seconds, housekeeping every
// minute.
func (w *Worker) Run(ctx context.Context) {
	events := time.NewTicker(eventInterval)
	housekeeping := time.NewTicker(housekeepingInterval)
	defer events.Stop()
	defer housekeeping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-events.C:
			if _, err := w.ProcessEvents(ctx); err != nil {
				slog.Error("processing events", "error", err)
			}
		case <-housekeeping.C:
			for task, fn := range map[string]func(context.Context) error{
				"expire unpaid orders": func(ctx context.Context) error { _, err := w.ExpireUnpaidOrders(ctx); return err },
				"expire carts":         func(ctx context.Context) error { _, err := w.ExpireCarts(ctx); return err },
				"purge":                w.Purge,
				"check senders":        func(ctx context.Context) error { _, err := w.CheckSenders(ctx); return err },
			} {
				if err := fn(ctx); err != nil {
					slog.Error("housekeeping", "task", task, "error", err)
				}
			}
		}
	}
}

// beginAsSite is a transaction acting as aether_app within one site.
func (w *Worker) beginAsSite(ctx context.Context, siteID uuid.UUID) (pgx.Tx, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := actAsSite(ctx, tx, siteID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func actAsSite(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE aether_app"); err != nil {
		return err
	}
	return db.ScopeToSite(ctx, tx, siteID)
}

// ProcessEvents delivers due events until none is left and returns how many
// were handled.
func (w *Worker) ProcessEvents(ctx context.Context) (int, error) {
	handled := 0
	for {
		more, err := w.processOne(ctx)
		if err != nil || !more {
			return handled, err
		}
		handled++
	}
}

func (w *Worker) processOne(ctx context.Context) (bool, error) {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := db.New(tx)
	event, err := q.NextDueEvent(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := actAsSite(ctx, tx, event.SiteID); err != nil {
		return false, err
	}
	if err := w.handle(ctx, q, event.Topic, event.Payload); err != nil {
		_ = tx.Rollback(ctx)
		attempts := event.Attempts + 1
		slog.Warn("event failed", "topic", event.Topic, "attempts", attempts, "error", err)
		return true, db.New(w.Pool).RetryEvent(ctx, db.RetryEventParams{
			ID: event.ID, Attempts: attempts, LastError: err.Error(), MaxAttempts: maxAttempts,
		})
	}
	if err := q.PublishEvent(ctx, event.ID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func (w *Worker) handle(ctx context.Context, q *db.Queries, topic string, raw json.RawMessage) error {
	var payload struct {
		OrderID    uuid.UUID `json:"order_id"`
		ShipmentID uuid.UUID `json:"shipment_id"`
		RefundID   uuid.UUID `json:"refund_id"`
		Purpose    string    `json:"purpose"`
		To         string    `json:"to"`
		Code       string    `json:"code"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return err
	}
	site, err := loadSite(ctx, q)
	if err != nil {
		return err
	}
	shop := site.Name
	switch topic {
	case "order.placed":
		order, err := loadOrder(ctx, q, payload.OrderID, customerOrderView)
		if err != nil {
			return err
		}
		return w.send(ctx, q, order.Email, orderConfirmation(shop, order))
	case "order.shipped":
		order, err := loadOrder(ctx, q, payload.OrderID, customerOrderView)
		if err != nil {
			return err
		}
		for _, s := range order.Shipments {
			if s.Id == payload.ShipmentID {
				return w.send(ctx, q, order.Email, orderShipped(shop, order, s))
			}
		}
		return nil
	case "order.refunded":
		order, err := loadOrder(ctx, q, payload.OrderID, customerOrderView)
		if err != nil {
			return err
		}
		amount, err := q.RefundAmount(ctx, payload.RefundID)
		if err != nil {
			return err
		}
		return w.send(ctx, q, order.Email, orderRefunded(shop, order, money(amount, order.Currency)))
	case "account.code":
		if payload.To == "" || payload.Code == "" {
			return nil
		}
		return w.send(ctx, q, payload.To, accountCode(shop, payload.Purpose, payload.Code))
	}
	return nil
}

// send sends from the site's verified domain; until it has one, from the
// provider fallback address under the shop's name (replies go to the
// merchant's reply-to). With neither, nothing is sent.
func (w *Worker) send(ctx context.Context, q *db.Queries, to string, mail rendered) error {
	sender, err := q.SenderOfSite(ctx)
	if err != nil {
		return err
	}
	name := strings.NewReplacer("<", "", ">", "", `"`, "").Replace(deref(sender.FromName, sender.SiteName))
	var address string
	switch {
	case deref(sender.Status, "") == "verified" && sender.FromLocal != nil && sender.Domain != nil:
		address = *sender.FromLocal + "@" + *sender.Domain
	case w.FallbackFrom != "":
		address = w.FallbackFrom
	default:
		slog.Info("no verified sending domain and no fallback; email not sent", "subject", mail.subject)
		return nil
	}
	email := ports.Email{From: fmt.Sprintf("%s <%s>", name, address), ReplyTo: deref(sender.ReplyTo, ""), To: to,
		Subject: mail.subject, Text: mail.text, HTML: mail.html}
	err = w.Mailer.Send(ctx, email)
	var provErr *ports.ProviderError
	if errors.As(err, &provErr) && provErr.Kind == ports.Rejected {
		slog.Warn("email refused; not retrying", "reason", provErr.Message, "subject", email.Subject)
		return nil
	}
	return err
}

// ExpireUnpaidOrders cancels orders not paid in time on sites that take
// online payments. An order whose payment went through meanwhile is left
// alone.
func (w *Worker) ExpireUnpaidOrders(ctx context.Context) (int, error) {
	due, err := db.New(w.Pool).DueUnpaidOrders(ctx, unpaidMinutes)
	if err != nil {
		return 0, err
	}
	cancelled := 0
	for _, o := range due {
		tx, err := w.beginAsSite(ctx, o.SiteID)
		if err != nil {
			return cancelled, err
		}
		err = cancelOrder(ctx, db.New(tx), w.Payments, o.SiteID, o.ID, ptr("not paid in time"), nil)
		var apiErr *APIError
		switch {
		case err == nil:
			if err := tx.Commit(ctx); err != nil {
				return cancelled, err
			}
			cancelled++
		case errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict:
			slog.Info("left unpaid order alone", "order_id", o.ID, "reason", apiErr.Detail)
		default:
			slog.Warn("cancelling unpaid order", "order_id", o.ID, "error", err)
		}
		_ = tx.Rollback(ctx)
	}
	return cancelled, nil
}

func (w *Worker) ExpireCarts(ctx context.Context) (int64, error) {
	return db.New(w.Pool).ExpireCarts(ctx)
}

// Purge deletes expired idempotency keys, sessions and codes, and events
// delivered over a week ago.
func (w *Worker) Purge(ctx context.Context) error {
	q := db.New(w.Pool)
	for _, f := range []func() error{
		func() error { return q.PurgeIdempotencyKeys(ctx) },
		func() error { return q.PurgeEvents(ctx, outboxRetentionDays) },
		func() error { return q.PurgeSessions(ctx) },
		func() error { return q.PurgeCodes(ctx) },
	} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

// CheckSenders asks the email provider about sending domains still
// pending verification; returns how many are now verified.
func (w *Worker) CheckSenders(ctx context.Context) (int, error) {
	pending, err := db.New(w.Pool).PendingSenders(ctx, senderCheckMinutes)
	if err != nil {
		return 0, err
	}
	verified := 0
	for _, s := range pending {
		status, err := w.Mailer.DomainStatus(ctx, s.Domain)
		if err != nil {
			slog.Warn("checking sending domain", "domain", s.Domain, "error", err)
			continue
		}
		if status == ports.DomainVerified {
			verified++
		}
		tx, err := w.beginAsSite(ctx, s.SiteID)
		if err != nil {
			return verified, err
		}
		if err := db.New(tx).SetSenderStatus(ctx, string(status)); err != nil {
			_ = tx.Rollback(ctx)
			return verified, err
		}
		if err := tx.Commit(ctx); err != nil {
			return verified, err
		}
	}
	return verified, nil
}
