package core_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"aether/internal/apitest"
)

// cartGuest is a guest shopper holding a cart token.
type cartGuest struct {
	api   *apitest.API
	shop  apitest.Shop
	token string
}

func cartStart(t *testing.T, api *apitest.API, shop apitest.Shop) cartGuest {
	t.Helper()
	status, body := api.Post(shop.Store("/carts"), "", apitest.M{})
	api.Want(status, http.StatusCreated, body)
	return cartGuest{api: api, shop: shop, token: body.Get("cart_token").Str()}
}

func (g cartGuest) call(method, path string, body any) (int, apitest.J) {
	return g.api.Send(g.api.Request(method, g.shop.Store(path), body, "x-cart-token", g.token))
}

func (g cartGuest) cart() apitest.J {
	g.api.T.Helper()
	status, cart := g.call(http.MethodGet, "/carts/current", nil)
	g.api.Want(status, http.StatusOK, cart)
	return cart
}

func (g cartGuest) add(body apitest.M) (int, apitest.J) {
	return g.call(http.MethodPost, "/carts/current/items", body)
}

func cartSetup(t *testing.T) (*apitest.API, apitest.Shop, apitest.J) {
	api := apitest.New(t)
	shop := api.OpenShop()
	return api, shop, shop.PadThai()
}

func TestLargeSpicyPadThaiIs21(t *testing.T) {
	api, shop, product := cartSetup(t)
	guest := cartStart(t, api, shop)
	spicy := apitest.OptionID(product, "spice", "spicy")

	status, cart := guest.add(apitest.M{"variant_id": apitest.VariantID(product, "Large"), "option_value_ids": []string{spicy}})

	api.Want(status, http.StatusOK, cart)
	line := cart.Get("items", 0)
	if got := line.Get("unit_price").Str(); got != "21.00" {
		t.Errorf("unit_price %q", got)
	}
	want := []any{map[string]any{"group": "Spice", "value_id": spicy, "label": "Spicy", "price_extra": "2.00"}}
	if !reflect.DeepEqual(line.Get("options").V, want) {
		t.Errorf("options %s", line.Get("options"))
	}
	if cart.Get("subtotal").Str() != "21.00" || cart.Get("ready").V != true {
		t.Errorf("cart %s", cart)
	}
}

func TestSameCombinationMergesAndDifferentOptionsSplit(t *testing.T) {
	api, shop, product := cartSetup(t)
	guest := cartStart(t, api, shop)
	regular := apitest.VariantID(product, "Regular")
	mild := apitest.OptionID(product, "spice", "mild")
	egg := apitest.OptionID(product, "extras", "egg")
	peanuts := apitest.OptionID(product, "extras", "peanuts")

	guest.add(apitest.M{"variant_id": regular, "option_value_ids": []string{mild, egg, peanuts}})
	// Same choices in another order are the same line.
	guest.add(apitest.M{"variant_id": regular, "quantity": 2, "option_value_ids": []string{peanuts, mild, egg}})
	_, cart := guest.add(apitest.M{"variant_id": regular, "option_value_ids": []string{mild}})

	lines := cart.Get("items")
	if lines.Len() != 2 {
		t.Fatalf("lines %s", lines)
	}
	if lines.Get(0, "quantity").Num() != 3 || lines.Get(0, "unit_price").Str() != "17.00" {
		t.Errorf("first line %s", lines.Get(0))
	}
	if lines.Get(0, "line_total").Str() != "51.00" {
		t.Errorf("line_total %s", lines.Get(0, "line_total"))
	}
	if cart.Get("subtotal").Str() != "66.00" || cart.Get("item_count").Num() != 4 {
		t.Errorf("cart %s", cart)
	}
}

func TestAddOnRulesAreEnforced(t *testing.T) {
	api, shop, product := cartSetup(t)
	_, other := api.Post(shop.Admin("/products"), shop.Key, apitest.M{"slug": "soup", "name": "Soup", "price": "5", "status": "active",
		"option_groups": []apitest.M{{"code": "size", "name": "Size", "max_select": 1, "values": []apitest.M{{"code": "cup", "label": "Cup"}}}}})
	guest := cartStart(t, api, shop)
	regular := apitest.VariantID(product, "Regular")
	mild := apitest.OptionID(product, "spice", "mild")
	var extras []string
	for _, v := range []string{"egg", "peanuts", "lime"} {
		extras = append(extras, apitest.OptionID(product, "extras", v))
	}

	cases := []struct {
		options []string
		message string
	}{
		{[]string{}, "choose at least 1 from Spice"},
		{[]string{mild, extras[0], extras[1], extras[2]}, "choose at most 2 from Extras"},
		{[]string{mild, apitest.OptionID(other, "size", "cup")}, "does not belong"},
		{[]string{mild, mild}, "chosen twice"},
	}
	for _, c := range cases {
		status, problem := guest.add(apitest.M{"variant_id": regular, "option_value_ids": c.options})
		if status != http.StatusUnprocessableEntity || !strings.Contains(problem.Get("detail").Str(), c.message) {
			t.Errorf("%q: %d %s", c.message, status, problem)
		}
	}
	if status, body := guest.add(apitest.M{"variant_id": regular, "quantity": 0, "option_value_ids": []string{mild}}); status != http.StatusUnprocessableEntity {
		t.Errorf("quantity 0: %d %s", status, body)
	}
}

func TestLinesAreRepricedAndFlaggedFromTheLiveCatalog(t *testing.T) {
	api, shop, product := cartSetup(t)
	guest := cartStart(t, api, shop)
	mild := apitest.OptionID(product, "spice", "mild")
	guest.add(apitest.M{"variant_id": apitest.VariantID(product, "Regular"), "option_value_ids": []string{mild}})
	productURI := shop.Admin("/products/" + product.Get("id").Str())

	api.Patch(productURI, shop.Key, apitest.M{"price": "16"})
	if got := guest.cart().Get("subtotal").Str(); got != "16.00" {
		t.Errorf("subtotal after price change %q", got)
	}

	api.Patch(productURI, shop.Key, apitest.M{"status": "archived"})
	cart := guest.cart()
	if cart.Get("items", 0, "problem").Str() != "no longer available" {
		t.Errorf("problem %s", cart.Get("items", 0))
	}
	if cart.Get("subtotal").Str() != "0.00" || cart.Get("ready").V != false {
		t.Errorf("cart %s", cart)
	}
}

func TestTrackedStockLimitsTheCart(t *testing.T) {
	api, shop, product := cartSetup(t)
	large := apitest.VariantID(product, "Large")
	mild := apitest.OptionID(product, "spice", "mild")
	spicy := apitest.OptionID(product, "spice", "spicy")
	adjust := shop.Admin("/inventory/" + large + "/adjustments")
	api.Post(adjust, shop.Key, apitest.M{"quantity": 2, "kind": "receipt"})
	guest := cartStart(t, api, shop)

	status, problem := guest.add(apitest.M{"variant_id": large, "quantity": 3, "option_value_ids": []string{mild}})
	if status != http.StatusUnprocessableEntity || problem.Get("detail").Str() != "only 2 available" {
		t.Errorf("over stock: %d %s", status, problem)
	}
	// Stock counts across lines of the same variant.
	guest.add(apitest.M{"variant_id": large, "option_value_ids": []string{mild}})
	if status, body := guest.add(apitest.M{"variant_id": large, "option_value_ids": []string{spicy}}); status != http.StatusOK {
		t.Errorf("second unit: %d %s", status, body)
	}
	if status, body := guest.add(apitest.M{"variant_id": large, "option_value_ids": []string{spicy}}); status != http.StatusUnprocessableEntity {
		t.Errorf("third unit: %d %s", status, body)
	}

	api.Post(adjust, shop.Key, apitest.M{"quantity": -1, "kind": "adjustment"})
	cart := guest.cart()
	if cart.Get("items", 0, "problem").Str() != "only 1 available" || cart.Get("ready").V != false {
		t.Errorf("cart %s", cart)
	}
}

func TestQuantitiesChangeAndLinesGo(t *testing.T) {
	api, shop, product := cartSetup(t)
	guest := cartStart(t, api, shop)
	_, cart := guest.add(apitest.M{"variant_id": apitest.VariantID(product, "Regular"), "option_value_ids": []string{apitest.OptionID(product, "spice", "mild")}})
	line := cart.Get("items", 0, "id").Str()

	status, cart := guest.call(http.MethodPatch, "/carts/current/items/"+line, apitest.M{"quantity": 4})
	if status != http.StatusOK || cart.Get("subtotal").Str() != "60.00" {
		t.Errorf("patch: %d %s", status, cart)
	}

	status, cart = guest.call(http.MethodDelete, "/carts/current/items/"+line, nil)
	if status != http.StatusOK || cart.Get("items").V == nil || cart.Get("items").Len() != 0 {
		t.Errorf("delete: %d %s", status, cart)
	}
	if cart.Get("ready").V != false {
		t.Errorf("ready %s", cart.Get("ready"))
	}
	if status, _ := guest.call(http.MethodDelete, "/carts/current/items/"+line, nil); status != http.StatusNotFound {
		t.Errorf("second delete: %d", status)
	}
}

func TestACartAnswersOnlyToItsTokenAndSite(t *testing.T) {
	api, shop, _ := cartSetup(t)
	other := api.OpenShop()
	guest := cartStart(t, api, shop)
	current := shop.Store("/carts/current")

	if status, _ := api.Get(current, ""); status != http.StatusNotFound {
		t.Errorf("no token: %d", status)
	}
	if status, _ := api.Send(api.Request(http.MethodGet, current, nil, "x-cart-token", strings.Repeat("0", 64))); status != http.StatusNotFound {
		t.Errorf("wrong token: %d", status)
	}
	if status, _ := api.Send(api.Request(http.MethodGet, other.Store("/carts/current"), nil, "x-cart-token", guest.token)); status != http.StatusNotFound {
		t.Errorf("other site: %d", status)
	}
	if items := guest.cart().Get("items"); !reflect.DeepEqual(items.V, []any{}) {
		t.Errorf("items %s", items)
	}
}
