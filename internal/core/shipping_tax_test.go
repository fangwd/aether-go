package core_test

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

func shipTaxAustin() apitest.M {
	return apitest.M{"line1": "1 Congress Ave", "city": "Austin", "state": "TX", "postal_code": "78701", "country_code": "US"}
}

func shipTaxLA() apitest.M {
	return apitest.M{"line1": "1 Main St", "city": "Los Angeles", "state": "CA", "postal_code": "90012", "country_code": "US"}
}

// shipTaxUSShop: Texas taxes 6.25% state + 2% local (stacked), the rest of
// the US 5%. Standard shipping is 10 nationally, 5 within Texas, free over 100.
func shipTaxUSShop(t *testing.T, api *apitest.API) apitest.Shop {
	t.Helper()
	shop := api.OpenShop()
	post := func(path string, body apitest.M) apitest.J {
		t.Helper()
		status, created := api.Post(shop.Admin(path), shop.Key, body)
		api.Want(status, http.StatusCreated, created)
		return created
	}
	standard := post("/tax-classes", apitest.M{"code": "standard", "name": "Standard"})
	for _, r := range []struct {
		region   any
		rate     string
		priority int
	}{{nil, "0.05", 0}, {"TX", "0.0625", 0}, {"TX", "0.02", 1}} {
		post("/tax-rates", apitest.M{"tax_class_id": standard.Get("id").V, "country_code": "US", "region_code": r.region,
			"rate": r.rate, "priority": r.priority})
	}
	domestic := post("/shipping-zones", apitest.M{"code": "us", "name": "US", "regions": []apitest.M{{"country_code": "US"}}})
	texas := post("/shipping-zones", apitest.M{"code": "tx", "name": "Texas",
		"regions": []apitest.M{{"country_code": "US", "region_code": "TX"}}})
	method := post("/shipping-methods", apitest.M{"code": "standard", "name": "Standard", "kind": "delivery",
		"tax_class_id": standard.Get("id").V})
	post("/shipping-methods", apitest.M{"code": "pickup", "name": "Pick up in store", "kind": "pickup"})
	for _, r := range []struct {
		zone       apitest.J
		min, price string
	}{{domestic, "0", "10"}, {domestic, "100", "0"}, {texas, "0", "5"}} {
		post("/shipping-rates", apitest.M{"method_id": method.Get("id").V, "zone_id": r.zone.Get("id").V,
			"min_order_amount": r.min, "price": r.price})
	}
	return shop
}

// shipTaxCart is a guest cart holding count Large Spicy Pad Thai (21.00 each).
func shipTaxCart(t *testing.T, api *apitest.API, shop apitest.Shop, count int) string {
	t.Helper()
	product := shop.PadThai()
	_, created := api.Post(shop.Store("/carts"), "", apitest.M{})
	token := created.Get("cart_token").Str()
	line := apitest.M{"variant_id": apitest.VariantID(product, "Large"), "quantity": count,
		"option_value_ids": []string{apitest.OptionID(product, "spice", "spicy")}}
	status, body := api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/items"), line, "x-cart-token", token))
	api.Want(status, http.StatusOK, body)
	return token
}

func shipTaxQuote(api *apitest.API, shop apitest.Shop, token string, body apitest.M) (int, apitest.J) {
	return api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/quote"), body, "x-cart-token", token))
}

func shipTaxCheckout(api *apitest.API, shop apitest.Shop, token string, body apitest.M) (int, apitest.J) {
	return api.Send(api.Request(http.MethodPost, shop.Store("/checkout"), body,
		"x-cart-token", token, "idempotency-key", uuid.Must(uuid.NewV7()).String()))
}

func TestTexasStacksTaxesAndGetsTheTexasRate(t *testing.T) {
	api := apitest.New(t)
	shop := shipTaxUSShop(t, api)
	token := shipTaxCart(t, api, shop, 2)

	status, q := shipTaxQuote(api, shop, token, apitest.M{"shipping_address": shipTaxAustin(), "shipping_method_code": "standard"})

	api.Want(status, http.StatusOK, q)
	var options [][2]string
	for _, o := range q.Get("shipping_options").Arr() {
		options = append(options, [2]string{o.Get("code").Str(), o.Get("price").Str()})
	}
	if !reflect.DeepEqual(options, [][2]string{{"pickup", "0.00"}, {"standard", "5.00"}}) {
		t.Errorf("options %v", options)
	}
	// 42.00 x 8.25% = 3.465 -> 3.47; shipping 5.00 x 8.25% = 0.41
	if q.Get("subtotal").Str() != "42.00" || q.Get("shipping_total").Str() != "5.00" {
		t.Errorf("subtotal/shipping %s", q)
	}
	if q.Get("shipping_tax").Str() != "0.41" || q.Get("tax_total").Str() != "3.88" {
		t.Errorf("taxes %s", q)
	}
	if q.Get("grand_total").Str() != "50.88" {
		t.Errorf("grand_total %s", q.Get("grand_total"))
	}
}

func TestElsewhereUsesTheNationalRateAndFreeShippingThreshold(t *testing.T) {
	api := apitest.New(t)
	shop := shipTaxUSShop(t, api)

	small := shipTaxCart(t, api, shop, 2)
	la := apitest.M{"shipping_address": shipTaxLA(), "shipping_method_code": "standard"}
	_, q := shipTaxQuote(api, shop, small, la)
	if q.Get("shipping_total").Str() != "10.00" || q.Get("tax_total").Str() != "2.60" || q.Get("grand_total").Str() != "54.60" {
		t.Errorf("small %s", q)
	}

	_, cart := api.Send(api.Request(http.MethodGet, shop.Store("/carts/current"), nil, "x-cart-token", small))
	line := cart.Get("items", 0)
	more := apitest.M{"variant_id": line.Get("variant_id").V, "quantity": 3, "option_value_ids": []any{line.Get("options", 0, "value_id").V}}
	api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/items"), more, "x-cart-token", small))
	_, big := shipTaxQuote(api, shop, small, la)
	if big.Get("subtotal").Str() != "105.00" || big.Get("shipping_total").Str() != "0.00" {
		t.Errorf("big %s", big)
	}
}

func TestWithoutAnAddressOnlyPickupIsOffered(t *testing.T) {
	api := apitest.New(t)
	shop := shipTaxUSShop(t, api)
	token := shipTaxCart(t, api, shop, 1)

	_, q := shipTaxQuote(api, shop, token, apitest.M{})
	var codes []string
	for _, o := range q.Get("shipping_options").Arr() {
		codes = append(codes, o.Get("code").Str())
	}
	if !reflect.DeepEqual(codes, []string{"pickup"}) {
		t.Errorf("codes %v", codes)
	}
	if status, body := shipTaxQuote(api, shop, token, apitest.M{"shipping_method_code": "standard"}); status != http.StatusUnprocessableEntity {
		t.Errorf("standard without address: %d %s", status, body)
	}
}

func TestCheckoutChargesExactlyTheQuote(t *testing.T) {
	api := apitest.New(t)
	shop := shipTaxUSShop(t, api)
	token := shipTaxCart(t, api, shop, 2)

	status, problem := shipTaxCheckout(api, shop, token, apitest.M{"email": "a@b.co", "shipping_address": shipTaxAustin()})
	if status != http.StatusUnprocessableEntity || !strings.Contains(problem.Get("detail").Str(), "shipping method") {
		t.Errorf("no method: %d %s", status, problem)
	}

	status, placed := shipTaxCheckout(api, shop, token, apitest.M{"email": "a@b.co", "shipping_address": shipTaxAustin(),
		"shipping_method_code": "standard"})
	api.Want(status, http.StatusCreated, placed)
	order := placed.Get("order")
	if order.Get("shipping_method_code").Str() != "standard" || order.Get("grand_total").Str() != "50.88" {
		t.Errorf("order %s", order)
	}
	if order.Get("shipping_tax").Str() != "0.41" || order.Get("tax_total").Str() != "3.88" {
		t.Errorf("taxes %s", order)
	}
	if order.Get("items", 0, "tax_total").Str() != "3.47" || order.Get("items", 0, "total").Str() != "45.47" {
		t.Errorf("item %s", order.Get("items", 0))
	}
}

func TestGstInclusivePickupIsTaxedWhereTheBusinessIs(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	owner := shop.Key
	status, body := api.Put(shop.Store(""), owner, apitest.M{"name": "Cafe", "currency": "AUD", "timezone": "Australia/Sydney",
		"prices_include_tax": true, "country_code": "AU", "region_code": "NSW"})
	api.Want(status, http.StatusOK, body)
	_, gst := api.Post(shop.Admin("/tax-classes"), owner, apitest.M{"code": "standard", "name": "GST"})
	api.Post(shop.Admin("/tax-rates"), owner, apitest.M{"tax_class_id": gst.Get("id").V, "country_code": "AU", "rate": "0.1"})
	api.Post(shop.Admin("/shipping-methods"), owner, apitest.M{"code": "pickup", "name": "Pick up", "kind": "pickup"})
	token := shipTaxCart(t, api, shop, 1)

	status, placed := shipTaxCheckout(api, shop, token, apitest.M{"email": "a@b.co", "shipping_method_code": "pickup"})

	api.Want(status, http.StatusCreated, placed)
	order := placed.Get("order")
	// 21.00 includes GST of 21 - 21/1.1 = 1.909 -> 1.91; nothing is added.
	if order.Get("tax_total").Str() != "1.91" || order.Get("grand_total").Str() != "21.00" {
		t.Errorf("order %s", order)
	}
	if order.Get("items", 0, "total").Str() != "21.00" {
		t.Errorf("item total %s", order.Get("items", 0, "total"))
	}
}

func TestConfigurationIsValidated(t *testing.T) {
	api := apitest.New(t)
	shop := shipTaxUSShop(t, api)
	_, classes := api.Get(shop.Admin("/tax-classes"), shop.Key)
	standard := classes.Get(0, "id").Str()
	_, methods := api.Get(shop.Admin("/shipping-methods"), shop.Key)
	pickup := methods.Find("kind", "pickup").Get("id").V
	_, zones := api.Get(shop.Admin("/shipping-zones"), shop.Key)

	bad := []struct {
		path string
		body apitest.M
	}{
		{"/tax-rates", apitest.M{"tax_class_id": standard, "rate": "1.5"}},
		{"/tax-rates", apitest.M{"tax_class_id": standard, "region_code": "TX", "rate": "0.1"}},
		{"/shipping-rates", apitest.M{"method_id": pickup, "zone_id": zones.Get(0, "id").V, "price": "5"}},
		{"/shipping-zones", apitest.M{"code": "x", "name": "X", "regions": []any{}}},
	}
	for _, b := range bad {
		if status, problem := api.Post(shop.Admin(b.path), shop.Key, b.body); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", b.path, status, problem)
		}
	}
	// The standard class is used by the shipping method: deleting it conflicts.
	if status, body := api.Delete(shop.Admin("/tax-classes/"+standard), shop.Key); status != http.StatusConflict {
		t.Errorf("delete used class: %d %s", status, body)
	}
}
