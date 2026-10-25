package core_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
	"aether/internal/core"
	"aether/internal/ports"
)

type workerWorld struct {
	t       *testing.T
	api     *apitest.API
	worker  *core.Worker
	shop    apitest.Shop
	product apitest.J
}

func newWorkerWorld(t *testing.T) *workerWorld {
	t.Helper()
	api := apitest.New(t)
	shop := api.OpenShop()
	return &workerWorld{t: t, api: api, worker: &core.Worker{Pool: api.Worker, Mailer: api.Mailer},
		shop: shop, product: shop.PadThai()}
}

func (w *workerWorld) verifySender() {
	w.t.Helper()
	body := apitest.M{"domain": "mail.padthai.example", "from_name": "Pad Thai Co", "reply_to": "hello@padthai.example"}
	status, sender := w.api.Put(w.shop.Admin("/email-sender"), w.shop.Key, body)
	if status != http.StatusOK || sender.Get("status").Str() != "pending" {
		w.t.Fatalf("register sender: %d %s", status, sender)
	}
	if sender.Get("dns_records", 0, "type").Str() != "CNAME" {
		w.t.Errorf("dns records: %s", sender.Get("dns_records"))
	}
	_, sender = w.api.Get(w.shop.Admin("/email-sender"), w.shop.Key)
	if sender.Get("status").Str() != "verified" {
		w.t.Fatalf("sender: %s", sender)
	}
}

// order is a guest order for two Large Spicy Pad Thai.
func (w *workerWorld) order() apitest.J {
	w.t.Helper()
	_, cart := w.api.Post(w.shop.Store("/carts"), "", apitest.M{})
	token := cart.Get("cart_token").Str()
	line := apitest.M{"variant_id": apitest.VariantID(w.product, "Large"), "quantity": 2,
		"option_value_ids": []string{apitest.OptionID(w.product, "spice", "spicy")}}
	w.api.Send(w.api.Request(http.MethodPost, w.shop.Store("/carts/current/items"), line, "x-cart-token", token))
	status, placed := w.api.Send(w.api.Request(http.MethodPost, w.shop.Store("/checkout"),
		apitest.M{"email": "ann@example.com"}, "x-cart-token", token, "idempotency-key", uuid.NewString()))
	w.api.Want(status, http.StatusCreated, placed)
	return placed.Get("order")
}

func (w *workerWorld) sent() []ports.Email { return w.api.Mailer.Sent() }

func (w *workerWorld) last() ports.Email {
	w.t.Helper()
	sent := w.sent()
	if len(sent) == 0 {
		w.t.Fatal("nothing sent")
	}
	return sent[len(sent)-1]
}

func (w *workerWorld) process() int {
	w.t.Helper()
	n, err := w.worker.ProcessEvents(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *workerWorld) exec(sql string) {
	w.t.Helper()
	if _, err := w.api.Admin.Exec(context.Background(), sql); err != nil {
		w.t.Fatal(err)
	}
}

func (w *workerWorld) unpublished() int {
	w.t.Helper()
	var n int
	if err := w.api.Admin.QueryRow(context.Background(), "SELECT count(*) FROM outbox_event WHERE published_at IS NULL").Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func TestWithoutAVerifiedDomainOrFallbackNothingIsSent(t *testing.T) {
	w := newWorkerWorld(t)
	w.order()

	if n := w.process(); n != 1 {
		t.Errorf("processed %d", n)
	}

	if sent := w.sent(); len(sent) != 0 {
		t.Errorf("sent: %+v", sent)
	}
	if n := w.unpublished(); n != 0 {
		t.Errorf("the event is done, not retried: %d unpublished", n)
	}
}

func TestUnverifiedShopsSendFromProviderUntilTheirDomainIsVerified(t *testing.T) {
	w := newWorkerWorld(t)
	w.worker.FallbackFrom = "shops@provider.example"
	// Domain registered but not verified yet: the provider sends, replies go to the shop.
	body := apitest.M{"domain": "mail.padthai.example", "from_name": "Pad Thai Co", "reply_to": "hello@padthai.example"}
	_, sender := w.api.Put(w.shop.Admin("/email-sender"), w.shop.Key, body)
	if sender.Get("status").Str() != "pending" {
		t.Fatalf("sender: %s", sender)
	}
	w.order()
	w.process()
	early := w.last()
	if early.From != "Pad Thai Co <shops@provider.example>" || early.ReplyTo != "hello@padthai.example" {
		t.Errorf("early: from %q reply-to %q", early.From, early.ReplyTo)
	}

	w.verifySender()
	w.order()
	w.process()
	if from := w.last().From; from != "Pad Thai Co <orders@mail.padthai.example>" {
		t.Errorf("verified from: %q", from)
	}
}

func TestAShopWithoutAnySenderSettingsUsesItsNameOnTheFallback(t *testing.T) {
	w := newWorkerWorld(t)
	w.worker.FallbackFrom = "shops@provider.example"
	w.order()

	w.process()

	email := w.last()
	if email.From != "Shop <shops@provider.example>" || email.ReplyTo != "" {
		t.Errorf("from %q reply-to %q", email.From, email.ReplyTo)
	}
}

func TestAVerifiedShopSendsConfirmationAndShippingEmails(t *testing.T) {
	w := newWorkerWorld(t)
	w.verifySender()
	order := w.order()
	w.process()

	sent := w.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d: %+v", len(sent), sent)
	}
	confirmation := sent[0]
	if confirmation.From != "Pad Thai Co <orders@mail.padthai.example>" {
		t.Errorf("from: %q", confirmation.From)
	}
	if confirmation.To != "ann@example.com" || confirmation.ReplyTo != "hello@padthai.example" {
		t.Errorf("to %q reply-to %q", confirmation.To, confirmation.ReplyTo)
	}
	if confirmation.Subject != "Shop: order #1 received" {
		t.Errorf("subject: %q", confirmation.Subject)
	}
	if !strings.Contains(confirmation.Text, "2 x Pad Thai") || !strings.Contains(confirmation.Text, "Spicy") {
		t.Errorf("text: %s", confirmation.Text)
	}
	if !strings.Contains(confirmation.Text, "Total 42.00 USD") {
		t.Errorf("total missing: %s", confirmation.Text)
	}

	shipments := w.shop.Admin(fmt.Sprintf("/orders/%s/shipments", order.Get("id").Str()))
	w.api.Post(shipments, w.shop.Key, apitest.M{"carrier": "AusPost", "tracking_number": "AP123"})
	w.process()
	shipped := w.last()
	if shipped.Subject != "Shop: order #1 is on its way" {
		t.Errorf("subject: %q", shipped.Subject)
	}
	if !strings.Contains(shipped.Text, "AusPost AP123") {
		t.Errorf("text: %s", shipped.Text)
	}
}

func TestAccountCodesAreEmailedAndNotKept(t *testing.T) {
	w := newWorkerWorld(t)
	w.verifySender()
	w.shop.UnverifiedShopper("ann@example.com")
	code := w.shop.EmailedCode("ann@example.com")

	w.process()

	email := w.last()
	if email.To != "ann@example.com" || email.Subject != "Shop: confirm your email" {
		t.Errorf("to %q subject %q", email.To, email.Subject)
	}
	if !strings.Contains(email.Text, code) {
		t.Errorf("code %q not in: %s", code, email.Text)
	}
	var kept *string
	if err := w.api.Admin.QueryRow(context.Background(),
		"SELECT payload->>'code' FROM outbox_event WHERE topic = 'account.code'").Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != nil {
		t.Errorf("code kept: %q", *kept)
	}
}

func TestUnpaidOrdersAreCancelledOnlyWherePaymentIsOnline(t *testing.T) {
	w := newWorkerWorld(t)
	large := apitest.VariantID(w.product, "Large")
	w.api.Post(w.shop.Admin(fmt.Sprintf("/inventory/%s/adjustments", large)), w.shop.Key,
		apitest.M{"quantity": 5, "kind": "receipt"})
	order := w.order()
	w.exec(`UPDATE "order" SET placed_at = now() - interval '31 minutes'`)

	// A shop without online payments keeps the order (pay on pickup).
	if n, err := w.worker.ExpireUnpaidOrders(context.Background()); err != nil || n != 0 {
		t.Errorf("without payments: %d %v", n, err)
	}
	w.exec("UPDATE site SET payments_enabled = true")
	if n, err := w.worker.ExpireUnpaidOrders(context.Background()); err != nil || n != 1 {
		t.Errorf("with payments: %d %v", n, err)
	}

	_, cancelled := w.api.Get(w.shop.Admin(fmt.Sprintf("/orders/%s", order.Get("id").Str())), w.shop.Key)
	if cancelled.Get("fulfilment_status").Str() != "cancelled" {
		t.Errorf("order: %s", cancelled)
	}
	if reason := cancelled.Get("history", -1, "reason").Str(); reason != "not paid in time" {
		t.Errorf("history: %s", cancelled.Get("history"))
	}
	_, levels := w.api.Get(w.shop.Admin("/inventory"), w.shop.Key)
	if levels.Get(0, "reserved").V != float64(0) {
		t.Errorf("the held stock is back: %s", levels)
	}
}

func TestStaleCartsExpire(t *testing.T) {
	w := newWorkerWorld(t)
	_, cart := w.api.Post(w.shop.Store("/carts"), "", apitest.M{})
	w.exec("UPDATE cart SET expires_at = now() - interval '1 minute'")

	if n, err := w.worker.ExpireCarts(context.Background()); err != nil || n != 1 {
		t.Errorf("expired %d %v", n, err)
	}

	read := w.api.Request(http.MethodGet, w.shop.Store("/carts/current"), nil, "x-cart-token", cart.Get("cart_token").Str())
	if status, _ := w.api.Send(read); status != http.StatusNotFound {
		t.Errorf("read an expired cart: %d", status)
	}
}

func TestFailedDeliveriesAreRetriedWithBackoff(t *testing.T) {
	w := newWorkerWorld(t)
	w.verifySender()
	w.order()
	w.worker.Mailer = apitest.FailingMailer{Err: ports.Unavailablef("down")}

	if n := w.process(); n != 1 {
		t.Errorf("processed %d", n)
	}

	var attempts int
	var dueLater bool
	if err := w.api.Admin.QueryRow(context.Background(),
		"SELECT attempts, next_retry_at > now() FROM outbox_event WHERE topic = 'order.placed'").Scan(&attempts, &dueLater); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || !dueLater {
		t.Errorf("attempts %d, due later %v", attempts, dueLater)
	}
	if n := w.process(); n != 0 {
		t.Errorf("not due again yet: processed %d", n)
	}
}

func TestADomainBelongsToOneSite(t *testing.T) {
	w := newWorkerWorld(t)
	w.verifySender()
	other := w.api.OpenShop()

	body := apitest.M{"domain": "Mail.PadThai.example", "from_name": "Impostor"}
	status, problem := w.api.Put(other.Admin("/email-sender"), other.Key, body)

	if status != http.StatusConflict {
		t.Errorf("%d %s", status, problem)
	}
}

func TestRefusedEmailsAreNotRetried(t *testing.T) {
	w := newWorkerWorld(t)
	w.verifySender()
	w.order()
	w.worker.Mailer = apitest.FailingMailer{Err: ports.Reject("bad address")}

	if n := w.process(); n != 1 {
		t.Errorf("processed %d", n)
	}

	if n := w.unpublished(); n != 0 {
		t.Errorf("the event is done, not retried: %d unpublished", n)
	}
}

type catalogHook struct{ sites []uuid.UUID }

func (h *catalogHook) CatalogChanged(_ context.Context, site uuid.UUID) error {
	h.sites = append(h.sites, site)
	return nil
}

func TestCatalogChangesAreAnnouncedOncePerQuietPeriod(t *testing.T) {
	w := newWorkerWorld(t) // creating Pad Thai is the first change
	hook := &catalogHook{}
	w.worker.CatalogHook = hook
	ctx := context.Background()
	id := w.product.Get("id").Str()
	w.api.Patch(w.shop.Admin("/products/"+id), w.shop.Key, apitest.M{"name": "Pad See Ew"})
	w.api.Post(w.shop.Admin("/categories"), w.shop.Key, apitest.M{"slug": "noodles", "name": "Noodles"})

	if _, err := w.worker.ProcessEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if len(hook.sites) != 0 {
		t.Fatalf("announced before the quiet period: %v", hook.sites)
	}

	if _, err := w.api.Worker.Exec(ctx, "UPDATE outbox_event SET next_retry_at = now() WHERE topic = 'catalog.changed'"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.worker.ProcessEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if len(hook.sites) != 1 || hook.sites[0].String() != w.shop.ID.String() {
		t.Fatalf("announced %v, want once for %s", hook.sites, w.shop.ID)
	}

	// Once announced, the next change queues a new notice.
	w.api.Patch(w.shop.Admin("/products/"+id), w.shop.Key, apitest.M{"price": "16.00"})
	var waiting int
	if err := w.api.Worker.QueryRow(ctx, "SELECT count(*) FROM outbox_event WHERE topic = 'catalog.changed' AND published_at IS NULL").Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if waiting != 1 {
		t.Errorf("waiting notices = %d, want 1", waiting)
	}
}
