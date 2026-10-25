package core_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
	"aether/internal/ports"
)

type payWorld struct {
	t       *testing.T
	api     *apitest.API
	gateway *apitest.FakeGateway
	shop    apitest.Shop
	product apitest.J
}

func newPayWorld(t *testing.T) *payWorld {
	t.Helper()
	api := apitest.New(t)
	gw := &apitest.FakeGateway{PaymentsEnabled: true}
	api.Server.Payments = gw
	api.Rebuild()
	shop := api.OpenShop()
	return &payWorld{t: t, api: api, gateway: gw, shop: shop, product: shop.PadThai()}
}

func (w *payWorld) connect() {
	w.t.Helper()
	body := apitest.M{"return_url": "https://editor.example/done", "refresh_url": "https://editor.example/retry"}
	status, link := w.api.Post(w.shop.Admin("/payments/onboarding"), w.shop.Key, body)
	if status != http.StatusOK || link.Get("url").Str() != "https://pay.example/onboard/acct_merchant" {
		w.t.Fatalf("onboarding: %d %s", status, link)
	}
	_, settings := w.api.Get(w.shop.Admin("/payments"), w.shop.Key)
	if settings.Get("payments_enabled").V != true {
		w.t.Fatalf("payments not enabled: %s", settings)
	}
}

func (w *payWorld) stock(quantity int) {
	large := apitest.VariantID(w.product, "Large")
	w.api.Post(w.shop.Admin(fmt.Sprintf("/inventory/%s/adjustments", large)), w.shop.Key,
		apitest.M{"quantity": quantity, "kind": "receipt"})
}

func (w *payWorld) onHand() [2]int {
	_, levels := w.api.Get(w.shop.Admin("/inventory"), w.shop.Key)
	return [2]int{int(levels.Get(0, "on_hand").Num()), int(levels.Get(0, "reserved").Num())}
}

// order is a guest order for one Large Spicy Pad Thai (21.00).
func (w *payWorld) order() (apitest.J, string) {
	w.t.Helper()
	_, cart := w.api.Post(w.shop.Store("/carts"), "", apitest.M{})
	token := cart.Get("cart_token").Str()
	line := apitest.M{"variant_id": apitest.VariantID(w.product, "Large"),
		"option_value_ids": []string{apitest.OptionID(w.product, "spice", "spicy")}}
	w.api.Send(w.api.Request(http.MethodPost, w.shop.Store("/carts/current/items"), line, "x-cart-token", token))
	status, placed := w.api.Send(w.api.Request(http.MethodPost, w.shop.Store("/checkout"),
		apitest.M{"email": "ann@example.com"}, "x-cart-token", token, "idempotency-key", uuid.NewString()))
	w.api.Want(status, http.StatusCreated, placed)
	return placed.Get("order"), placed.Get("order_token").Str()
}

func (w *payWorld) pay(order apitest.J, token string) (int, apitest.J) {
	uri := w.shop.Store(fmt.Sprintf("/orders/%s/payment", order.Get("id").Str()))
	return w.api.Send(w.api.Request(http.MethodPost, uri, nil, "x-order-token", token))
}

func (w *payWorld) webhook(event apitest.M) int {
	status, _ := w.api.Send(w.api.Request(http.MethodPost, "/v1/webhooks/payments/fake", event, "fake-signature", apitest.FakeSignature))
	return status
}

func (w *payWorld) event(kind apitest.M) apitest.M {
	return apitest.M{"id": "evt_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "account": apitest.FakeAccount,
		"site_id": w.shop.ID, "kind": kind}
}

func (w *payWorld) paidEvent(order apitest.J) apitest.M {
	return w.event(apitest.M{"type": "payment_succeeded", "order_id": order.Get("id").V, "reference": "pay_1"})
}

func (w *payWorld) adminOrder(order apitest.J) apitest.J {
	_, body := w.api.Get(w.shop.Admin(fmt.Sprintf("/orders/%s", order.Get("id").Str())), w.shop.Key)
	return body
}

func (w *payWorld) refundsURI(order apitest.J) string {
	return w.shop.Admin(fmt.Sprintf("/orders/%s/refunds", order.Get("id").Str()))
}

func (w *payWorld) cancelURI(order apitest.J) string {
	return w.shop.Admin(fmt.Sprintf("/orders/%s/cancel", order.Get("id").Str()))
}

func payJSONEqual(t *testing.T, got apitest.J, want string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(want), &v); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(got.V, v)
}

func TestConnectingPaymentsReusesTheAccount(t *testing.T) {
	w := newPayWorld(t)
	body := apitest.M{"return_url": "https://x/done", "refresh_url": "https://x/retry"}

	w.connect()
	// Resuming onboarding links the same account.
	if status, b := w.api.Post(w.shop.Admin("/payments/onboarding"), w.shop.Key, body); status != http.StatusOK {
		t.Errorf("resume onboarding: %d %s", status, b)
	}
	if _, site := w.api.Get(w.shop.Store(""), ""); site.Get("payments_enabled").V != true {
		t.Errorf("site: %s", site)
	}
	if calls := w.gateway.Calls(); !reflect.DeepEqual(calls, []string{"create_account"}) {
		t.Errorf("calls: %q", calls)
	}
}

func TestPayingNeedsAConnectedAccountAndReusesThePayment(t *testing.T) {
	w := newPayWorld(t)
	order, token := w.order()
	if status, b := w.pay(order, token); status != http.StatusConflict {
		t.Errorf("pay before connecting: %d %s", status, b)
	}

	w.connect()
	status, payment := w.pay(order, token)
	w.api.Want(status, http.StatusOK, payment)
	if !payJSONEqual(t, payment, `{"provider": "fake", "payment_reference": "pay_1", "amount": "21.00", "currency": "USD",
		"client": {"secret": "secret_1"}}`) {
		t.Errorf("payment: %s", payment)
	}
	if _, again := w.pay(order, token); again.Get("payment_reference").Str() != "pay_1" {
		t.Errorf("second pay: %s", again)
	}
	want := []string{"create_account", "start_payment 21.00 USD " + apitest.FakeAccount}
	if calls := w.gateway.Calls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("calls: %q", calls)
	}
	if status, _ := w.pay(order, "wrong"); status != http.StatusNotFound {
		t.Errorf("wrong token: %d", status)
	}
}

func TestASignedPaymentEventMarksTheOrderPaidOnce(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	w.stock(5)
	order, token := w.order()
	w.pay(order, token)
	if got := w.onHand(); got != [2]int{5, 1} {
		t.Fatalf("on hand after paying: %v", got)
	}

	event := w.paidEvent(order)
	if status := w.webhook(event); status != http.StatusOK {
		t.Errorf("webhook: %d", status)
	}
	if status := w.webhook(event); status != http.StatusOK {
		t.Errorf("replays are acknowledged: %d", status)
	}

	if paid := w.adminOrder(order); paid.Get("payment_status").Str() != "paid" {
		t.Errorf("order: %s", paid)
	}
	if got := w.onHand(); got != [2]int{4, 0} {
		t.Errorf("held stock became a sale exactly once: %v", got)
	}
	if status, _ := w.pay(order, token); status != http.StatusConflict {
		t.Errorf("pay a paid order: %d", status)
	}
}

func TestForgedOrForeignEventsChangeNothing(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	order, token := w.order()
	w.pay(order, token)

	forged := w.api.Request(http.MethodPost, "/v1/webhooks/payments/fake", w.paidEvent(order), "fake-signature", "forged")
	if status, _ := w.api.Send(forged); status != http.StatusBadRequest {
		t.Errorf("forged: %d", status)
	}
	foreign := w.paidEvent(order)
	foreign["account"] = "acct_someone_else"
	if status := w.webhook(foreign); status != http.StatusOK {
		t.Errorf("foreign: %d", status)
	}

	otherProvider := w.api.Request(http.MethodPost, "/v1/webhooks/payments/stripe", w.paidEvent(order), "fake-signature", apitest.FakeSignature)
	if status, _ := w.api.Send(otherProvider); status != http.StatusNotFound {
		t.Errorf("other provider: %d", status)
	}

	if o := w.adminOrder(order); o.Get("payment_status").Str() != "pending" {
		t.Errorf("order: %s", o)
	}
}

func TestPartialRefundsRestockAndAddUp(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	w.stock(5)
	order, token := w.order()
	w.pay(order, token)
	w.webhook(w.paidEvent(order))
	refunds := w.refundsURI(order)
	item := order.Get("items", 0, "id").Str()

	if status, b := w.api.Post(refunds, w.shop.Key, apitest.M{"amount": "21.01"}); status != http.StatusUnprocessableEntity {
		t.Errorf("too much: %d %s", status, b)
	}
	status, refunded := w.api.Post(refunds, w.shop.Key, apitest.M{"amount": "10.00", "reason": "cold",
		"items": []apitest.M{{"order_item_id": item, "quantity": 1}}})

	w.api.Want(status, http.StatusOK, refunded)
	if refunded.Get("payment_status").Str() != "partially_refunded" || refunded.Get("refunded_total").Str() != "10.00" {
		t.Errorf("refunded: %s", refunded)
	}
	if refunded.Get("items", 0, "refunded_quantity").Num() != 1 {
		t.Errorf("refunded quantity: %s", refunded.Get("items", 0))
	}
	if got := w.onHand(); got != [2]int{5, 0} {
		t.Errorf("the returned unit is back in stock: %v", got)
	}

	_, done := w.api.Post(refunds, w.shop.Key, apitest.M{"amount": "11.00"})
	if done.Get("payment_status").Str() != "refunded" || done.Get("refunded_total").Str() != "21.00" {
		t.Errorf("done: %s", done)
	}
	calls := w.gateway.Calls()
	if len(calls) < 2 || !reflect.DeepEqual(calls[2:], []string{"refund 10.00 of pay_1", "refund 11.00 of pay_1"}) {
		t.Errorf("calls: %q", calls)
	}
}

func TestAPendingRefundSettlesByEvent(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	order, token := w.order()
	w.pay(order, token)
	w.webhook(w.paidEvent(order))
	w.gateway.RefundStatuses = append(w.gateway.RefundStatuses, ports.RefundPending)

	_, pending := w.api.Post(w.refundsURI(order), w.shop.Key, apitest.M{"amount": "21.00"})
	if pending.Get("payment_status").Str() != "paid" {
		t.Errorf("pending refund: %s", pending)
	}
	if len(w.gateway.RefundIDs) == 0 {
		t.Fatal("no refund requested")
	}
	refundID := w.gateway.RefundIDs[0]
	event := w.event(apitest.M{"type": "refund_settled", "refund_id": refundID, "status": "succeeded"})
	if status := w.webhook(event); status != http.StatusOK {
		t.Errorf("webhook: %d", status)
	}

	if o := w.adminOrder(order); o.Get("payment_status").Str() != "refunded" {
		t.Errorf("order: %s", o)
	}
}

func TestCancellingStopsAnOpenPayment(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	order, token := w.order()
	w.pay(order, token)
	w.gateway.CancelResults = append(w.gateway.CancelResults, func() (ports.PaymentStatus, error) {
		return "", ports.Reject("already succeeded")
	})
	cancel := w.cancelURI(order)

	// The provider says the payment went through: the order must not be cancelled.
	if status, b := w.api.Post(cancel, w.shop.Key, apitest.M{}); status != http.StatusConflict {
		t.Errorf("first cancel: %d %s", status, b)
	}
	status, cancelled := w.api.Post(cancel, w.shop.Key, apitest.M{})
	if status != http.StatusOK || cancelled.Get("fulfilment_status").Str() != "cancelled" {
		t.Errorf("second cancel: %d %s", status, cancelled)
	}
	if status, _ := w.pay(order, token); status != http.StatusConflict {
		t.Errorf("pay a cancelled order: %d", status)
	}
	calls := w.gateway.Calls()
	if len(calls) < 2 || !reflect.DeepEqual(calls[2:], []string{"cancel_payment pay_1", "cancel_payment pay_1"}) {
		t.Errorf("calls: %q", calls)
	}
}

func TestAnOrderIsNotCancelledWhileItsPaymentIsInProgress(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	order, token := w.order()
	w.pay(order, token)
	w.gateway.CancelResults = append(w.gateway.CancelResults, func() (ports.PaymentStatus, error) {
		return ports.PaymentProcessing, nil
	})

	if status, b := w.api.Post(w.cancelURI(order), w.shop.Key, apitest.M{}); status != http.StatusConflict {
		t.Errorf("cancel: %d %s", status, b)
	}
	if o := w.adminOrder(order); o.Get("fulfilment_status").Str() != "unfulfilled" {
		t.Errorf("order: %s", o)
	}
}

func TestProviderRefusalsDoNotLeakTheProvidersMessage(t *testing.T) {
	w := newPayWorld(t)
	w.connect()
	order, token := w.order()
	w.gateway.StartError = ports.Reject("acct_merchant: internal detail")

	status, problem := w.pay(order, token)

	if status != http.StatusUnprocessableEntity || problem.Get("detail").Str() != "the provider refused the request" {
		t.Errorf("%d %s", status, problem)
	}
}

func TestPaymentsNeedAProviderConfigured(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	status, body := api.Get(shop.Admin("/payments"), shop.Key)

	if status != http.StatusServiceUnavailable {
		t.Errorf("%d %s", status, body)
	}
}
