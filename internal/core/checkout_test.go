package core_test

import (
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

type checkoutGuest struct {
	api   *apitest.API
	shop  apitest.Shop
	token string
}

func checkoutStart(api *apitest.API, shop apitest.Shop) checkoutGuest {
	_, body := api.Post(shop.Store("/carts"), "", apitest.M{})
	return checkoutGuest{api: api, shop: shop, token: body.Get("cart_token").Str()}
}

func (g checkoutGuest) add(variantID string, quantity int, options []string) {
	g.api.T.Helper()
	req := g.api.Request(http.MethodPost, g.shop.Store("/carts/current/items"),
		apitest.M{"variant_id": variantID, "quantity": quantity, "option_value_ids": options}, "x-cart-token", g.token)
	status, body := g.api.Send(req)
	g.api.Want(status, http.StatusOK, body)
}

func (g checkoutGuest) checkoutWith(key string, body any) (int, apitest.J) {
	return g.api.Send(g.api.Request(http.MethodPost, g.shop.Store("/checkout"), body,
		"x-cart-token", g.token, "idempotency-key", key))
}

func (g checkoutGuest) checkout() (int, apitest.J) {
	key, _ := uuid.NewV7()
	return g.checkoutWith(key.String(), checkoutDetails())
}

func checkoutDetails() apitest.M {
	return apitest.M{
		"email": "Ann@Example.com",
		"shipping_address": apitest.M{"first_name": "Ann", "line1": "1 George St", "city": "Sydney", "state": "NSW",
			"postal_code": "2000", "country_code": "AU"},
	}
}

func checkoutSetup(t *testing.T) (*apitest.API, apitest.Shop, apitest.J) {
	api := apitest.New(t)
	shop := api.OpenShop()
	return api, shop, shop.PadThai()
}

func TestCheckoutPlacesASnapshotOrder(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	guest := checkoutStart(api, shop)
	guest.add(apitest.VariantID(product, "Large"), 2, []string{apitest.OptionID(product, "spice", "spicy")})

	status, placed := guest.checkout()

	api.Want(status, http.StatusCreated, placed)
	order := placed.Get("order")
	if order.Get("order_number").Num() != 1 || order.Get("email").Str() != "ann@example.com" {
		t.Errorf("order %s", order)
	}
	if order.Get("subtotal").Str() != "42.00" || order.Get("grand_total").Str() != "42.00" {
		t.Errorf("totals %s", order)
	}
	if order.Get("payment_status").Str() != "pending" || order.Get("fulfilment_status").Str() != "unfulfilled" {
		t.Errorf("statuses %s", order)
	}
	item := order.Get("items", 0)
	if item.Get("unit_price").Str() != "21.00" || item.Get("total").Str() != "42.00" || item.Get("sku").Str() != "PT-L" {
		t.Errorf("item %s", item)
	}
	if item.Get("options", 0, "label").Str() != "Spicy" {
		t.Errorf("options %s", item.Get("options"))
	}
	if order.Get("shipping_address", "city").Str() != "Sydney" {
		t.Errorf("shipping %s", order.Get("shipping_address"))
	}
	if order.Get("billing_address", "city").Str() != "Sydney" {
		t.Errorf("billing defaults to shipping: %s", order.Get("billing_address"))
	}

	// The cart is spent, and the order token opens the order.
	if status, _ := api.Send(api.Request(http.MethodGet, shop.Store("/carts/current"), nil, "x-cart-token", guest.token)); status != http.StatusNotFound {
		t.Errorf("spent cart: %d", status)
	}
	uri := shop.Store("/orders/" + order.Get("id").Str())
	token := placed.Get("order_token").Str()
	status, read := api.Send(api.Request(http.MethodGet, uri, nil, "x-order-token", token))
	if status != http.StatusOK || read.Get("id").Str() != order.Get("id").Str() {
		t.Errorf("read: %d %s", status, read)
	}
	if m, _ := read.V.(map[string]any); m != nil {
		if _, ok := m["history"]; ok {
			t.Errorf("history shown to the shopper: %s", read)
		}
	}
	if status, _ := api.Send(api.Request(http.MethodGet, uri, nil, "x-order-token", "nope")); status != http.StatusNotFound {
		t.Errorf("wrong order token: %d", status)
	}
}

func TestRetriesWithTheSameKeyReturnTheSameOrder(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	guest := checkoutStart(api, shop)
	guest.add(apitest.VariantID(product, "Regular"), 1, []string{apitest.OptionID(product, "spice", "mild")})
	key := uuid.Must(uuid.NewV7()).String()

	firstStatus, first := guest.checkoutWith(key, checkoutDetails())
	secondStatus, second := guest.checkoutWith(key, checkoutDetails())
	if firstStatus != http.StatusCreated || secondStatus != http.StatusCreated {
		t.Fatalf("statuses %d %d: %s", firstStatus, secondStatus, second)
	}
	if first.Get("order", "id").Str() != second.Get("order", "id").Str() {
		t.Errorf("orders %s vs %s", first.Get("order", "id"), second.Get("order", "id"))
	}

	_, orders := api.Get(shop.Admin("/orders"), shop.Key)
	if orders.Get("items").Len() != 1 {
		t.Errorf("orders %s", orders)
	}

	// A new key on the spent cart is a conflict, not a second order.
	if status, body := guest.checkout(); status != http.StatusConflict {
		t.Errorf("new key on spent cart: %d %s", status, body)
	}
	// The key belongs to that cart only.
	other := checkoutStart(api, shop)
	other.add(apitest.VariantID(product, "Regular"), 1, []string{apitest.OptionID(product, "spice", "mild")})
	if status, body := other.checkoutWith(key, checkoutDetails()); status != http.StatusUnprocessableEntity {
		t.Errorf("key reused on another cart: %d %s", status, body)
	}
}

func TestCheckoutNeedsAKeyDetailsAndAReadyCart(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	guest := checkoutStart(api, shop)

	noKey := api.Request(http.MethodPost, shop.Store("/checkout"), checkoutDetails(), "x-cart-token", guest.token)
	if status, _ := api.Send(noKey); status != http.StatusBadRequest {
		t.Errorf("no key: %d", status)
	}
	status, problem := guest.checkout()
	if status != http.StatusUnprocessableEntity || problem.Get("detail").Str() != "the cart is empty" {
		t.Errorf("empty cart: %d %s", status, problem)
	}

	guest.add(apitest.VariantID(product, "Regular"), 1, []string{apitest.OptionID(product, "spice", "mild")})
	if status, body := guest.checkoutWith("k1", apitest.M{"email": "nope"}); status != http.StatusUnprocessableEntity {
		t.Errorf("bad email: %d %s", status, body)
	}
	badCountry := apitest.M{"email": "a@b.co", "shipping_address": apitest.M{"line1": "x", "country_code": "aus"}}
	if status, body := guest.checkoutWith("k2", badCountry); status != http.StatusUnprocessableEntity {
		t.Errorf("bad country: %d %s", status, body)
	}

	api.Patch(shop.Admin("/products/"+product.Get("id").Str()), shop.Key, apitest.M{"status": "archived"})
	status, problem = guest.checkout()
	if status != http.StatusUnprocessableEntity || !strings.Contains(problem.Get("detail").Str(), "no longer available") {
		t.Errorf("archived: %d %s", status, problem)
	}
}

func TestTheLastUnitGoesToExactlyOneCheckout(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	large := apitest.VariantID(product, "Large")
	mild := apitest.OptionID(product, "spice", "mild")
	api.Post(shop.Admin("/inventory/"+large+"/adjustments"), shop.Key, apitest.M{"quantity": 1, "kind": "receipt"})
	first := checkoutStart(api, shop)
	second := checkoutStart(api, shop)
	first.add(large, 1, []string{mild})
	second.add(large, 1, []string{mild})

	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, g := range []checkoutGuest{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], _ = g.checkout()
		}()
	}
	wg.Wait()

	// The loser sees "sold out" either while re-pricing (422) or while
	// reserving (409), depending on how the two interleave.
	if statuses[0] > statuses[1] {
		statuses[0], statuses[1] = statuses[1], statuses[0]
	}
	if statuses[0] != http.StatusCreated || (statuses[1] != http.StatusConflict && statuses[1] != http.StatusUnprocessableEntity) {
		t.Errorf("statuses %v", statuses)
	}
	_, levels := api.Get(shop.Admin("/inventory"), shop.Key)
	if levels.Get(0, "on_hand").Num() != 1 || levels.Get(0, "reserved").Num() != 1 {
		t.Errorf("levels %s", levels)
	}
}

func TestOrderNumbersCountPerSite(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	other := api.OpenShop()
	otherProduct := other.PadThai()
	var numbers []float64
	for _, c := range []struct {
		shop    apitest.Shop
		product apitest.J
	}{{shop, product}, {shop, product}, {other, otherProduct}} {
		guest := checkoutStart(api, c.shop)
		guest.add(apitest.VariantID(c.product, "Regular"), 1, []string{apitest.OptionID(c.product, "spice", "mild")})
		_, placed := guest.checkout()
		numbers = append(numbers, placed.Get("order", "order_number").Num())
	}
	if !reflect.DeepEqual(numbers, []float64{1, 2, 1}) {
		t.Errorf("numbers %v", numbers)
	}
}

func TestCancellingReleasesHeldStock(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	large := apitest.VariantID(product, "Large")
	api.Post(shop.Admin("/inventory/"+large+"/adjustments"), shop.Key, apitest.M{"quantity": 3, "kind": "receipt"})
	guest := checkoutStart(api, shop)
	guest.add(large, 2, []string{apitest.OptionID(product, "spice", "mild")})
	_, placed := guest.checkout()
	orderURI := shop.Admin("/orders/" + placed.Get("order", "id").Str())
	if _, levels := api.Get(shop.Admin("/inventory"), shop.Key); levels.Get(0, "reserved").Num() != 2 {
		t.Errorf("reserved before cancel %s", levels)
	}

	status, order := api.Post(orderURI+"/cancel", shop.Key, apitest.M{"reason": "customer asked"})

	api.Want(status, http.StatusOK, order)
	if order.Get("fulfilment_status").Str() != "cancelled" {
		t.Errorf("fulfilment_status %s", order.Get("fulfilment_status"))
	}
	var history []string
	for _, h := range order.Get("history").Arr() {
		history = append(history, h.Get("to_status").Str())
	}
	if !reflect.DeepEqual(history, []string{"pending", "unfulfilled", "cancelled"}) {
		t.Errorf("history %v", history)
	}
	if order.Get("history", 2, "actor").Str() != "provider:1" {
		t.Errorf("actor %s", order.Get("history", 2, "actor"))
	}
	if _, levels := api.Get(shop.Admin("/inventory"), shop.Key); levels.Get(0, "reserved").V != float64(0) {
		t.Errorf("reserved after cancel %s", levels)
	}
	if status, _ := api.Post(orderURI+"/cancel", shop.Key, apitest.M{}); status != http.StatusConflict {
		t.Errorf("second cancel: %d", status)
	}
}

func TestShipmentsFulfilAnOrderInParts(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	guest := checkoutStart(api, shop)
	guest.add(apitest.VariantID(product, "Regular"), 3, []string{apitest.OptionID(product, "spice", "mild")})
	_, placed := guest.checkout()
	order := placed.Get("order")
	shipments := shop.Admin("/orders/" + order.Get("id").Str() + "/shipments")
	item := order.Get("items", 0, "id").Str()

	if status, body := api.Post(shipments, shop.Key, apitest.M{"items": []apitest.M{{"order_item_id": item, "quantity": 4}}}); status != http.StatusUnprocessableEntity {
		t.Errorf("over-ship: %d %s", status, body)
	}

	status, partial := api.Post(shipments, shop.Key, apitest.M{"carrier": "auspost", "tracking_number": "T1",
		"items": []apitest.M{{"order_item_id": item, "quantity": 1}}})
	api.Want(status, http.StatusCreated, partial)
	if partial.Get("fulfilment_status").Str() != "partially_fulfilled" || partial.Get("items", 0, "shipped_quantity").Num() != 1 {
		t.Errorf("partial %s", partial)
	}

	_, done := api.Post(shipments, shop.Key, apitest.M{})
	if done.Get("fulfilment_status").Str() != "fulfilled" || done.Get("items", 0, "shipped_quantity").Num() != 3 {
		t.Errorf("done %s", done)
	}
	shipment := done.Get("shipments", 0, "id").Str()
	status, delivered := api.Patch(shipments+"/"+shipment, shop.Key, apitest.M{"status": "delivered"})
	if status != http.StatusOK || delivered.Get("shipments", 0, "status").Str() != "delivered" {
		t.Errorf("delivered: %d %s", status, delivered)
	}

	if status, _ := api.Post(shop.Admin("/orders/"+order.Get("id").Str()+"/cancel"), shop.Key, apitest.M{}); status != http.StatusConflict {
		t.Errorf("cancel after shipping: %d", status)
	}
}

func TestOrdersKeepTheirSnapshotWhenTheCatalogChanges(t *testing.T) {
	api, shop, product := checkoutSetup(t)
	guest := checkoutStart(api, shop)
	guest.add(apitest.VariantID(product, "Regular"), 1, []string{apitest.OptionID(product, "spice", "mild")})
	_, placed := guest.checkout()

	api.Patch(shop.Admin("/products/"+product.Get("id").Str()), shop.Key, apitest.M{"name": "Renamed", "price": "99"})

	_, order := api.Get(shop.Admin("/orders/"+placed.Get("order", "id").Str()), shop.Key)
	if order.Get("items", 0, "product_name").Str() != "Pad Thai" || order.Get("items", 0, "unit_price").Str() != "15.00" {
		t.Errorf("order item %s", order.Get("items", 0))
	}
	_, list := api.Get(shop.Admin("/orders")+"?payment_status=pending", shop.Key)
	if list.Get("items", 0, "grand_total").Str() != "15.00" {
		t.Errorf("list %s", list)
	}
}
