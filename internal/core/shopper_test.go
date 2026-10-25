package core_test

import (
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

func shopperSetup(t *testing.T) (*apitest.API, apitest.Shop, apitest.J) {
	api := apitest.New(t)
	shop := api.OpenShop()
	return api, shop, shop.PadThai()
}

func shopperLine(product apitest.J, size string, quantity int) apitest.M {
	return apitest.M{"variant_id": apitest.VariantID(product, size), "quantity": quantity,
		"option_value_ids": []string{apitest.OptionID(product, "spice", "mild")}}
}

func shopperCheckout(api *apitest.API, shop apitest.Shop, bearer, cartToken string, body any) (int, apitest.J) {
	headers := []string{"idempotency-key", uuid.Must(uuid.NewV7()).String()}
	if bearer != "" {
		headers = append(headers, "Authorization", "Bearer "+bearer)
	}
	if cartToken != "" {
		headers = append(headers, "x-cart-token", cartToken)
	}
	return api.Send(api.Request(http.MethodPost, shop.Store("/checkout"), body, headers...))
}

func shopperHasKey(j apitest.J, key string) bool {
	m, _ := j.V.(map[string]any)
	_, ok := m[key]
	return ok && m[key] != nil
}

func TestAShopperHasOneCartAndTheirOwnOrders(t *testing.T) {
	api, shop, product := shopperSetup(t)
	ann := shop.Shopper("ann@example.com")

	status, created := api.Post(shop.Store("/carts"), ann, apitest.M{})
	api.Want(status, http.StatusCreated, created)
	if shopperHasKey(created, "cart_token") {
		t.Errorf("a shopper's cart has a token: %s", created)
	}
	status, again := api.Post(shop.Store("/carts"), ann, apitest.M{})
	if status != http.StatusOK || again.Get("cart", "id").Str() != created.Get("cart", "id").Str() {
		t.Errorf("second cart: %d %s", status, again)
	}

	api.Post(shop.Store("/carts/current/items"), ann, shopperLine(product, "Large", 1))
	status, placed := shopperCheckout(api, shop, ann, "", apitest.M{})
	api.Want(status, http.StatusCreated, placed)
	if placed.Get("order", "email").Str() != "ann@example.com" {
		t.Errorf("email %s", placed.Get("order", "email"))
	}
	if shopperHasKey(placed, "order_token") {
		t.Errorf("a shopper's order has a token: %s", placed)
	}

	_, mine := api.Get(shop.Store("/orders"), ann)
	if mine.Len() != 1 {
		t.Errorf("mine %s", mine)
	}
	orderURI := shop.Store("/orders/" + placed.Get("order", "id").Str())
	if status, body := api.Get(orderURI, ann); status != http.StatusOK {
		t.Errorf("ann reads her order: %d %s", status, body)
	}
	bob := shop.Shopper("bob@example.com")
	if status, _ := api.Get(orderURI, bob); status != http.StatusNotFound {
		t.Errorf("bob reads ann's order: %d", status)
	}
	if _, theirs := api.Get(shop.Store("/orders"), bob); !reflect.DeepEqual(theirs.V, []any{}) {
		t.Errorf("bob's orders %s", theirs)
	}
}

func TestSigningInMergesTheGuestCart(t *testing.T) {
	api, shop, product := shopperSetup(t)
	ann := shop.Shopper("ann@example.com")
	api.Post(shop.Store("/carts"), ann, apitest.M{})
	api.Post(shop.Store("/carts/current/items"), ann, shopperLine(product, "Large", 1))

	_, guest := api.Post(shop.Store("/carts"), "", apitest.M{})
	token := guest.Get("cart_token").Str()
	for _, body := range []apitest.M{shopperLine(product, "Large", 2), shopperLine(product, "Regular", 1)} {
		api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/items"), body, "x-cart-token", token))
	}

	status, cart := api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/merge"), nil,
		"Authorization", "Bearer "+ann, "x-cart-token", token))

	api.Want(status, http.StatusOK, cart)
	var quantities []float64
	for _, l := range cart.Get("items").Arr() {
		quantities = append(quantities, l.Get("quantity").Num())
	}
	if !reflect.DeepEqual(quantities, []float64{3, 1}) {
		t.Errorf("quantities %v", quantities)
	}
	if status, _ := api.Send(api.Request(http.MethodGet, shop.Store("/carts/current"), nil, "x-cart-token", token)); status != http.StatusNotFound {
		t.Errorf("guest cart after merge: %d", status)
	}
}

func TestAShopperWithoutACartAdoptsTheGuestCart(t *testing.T) {
	api, shop, product := shopperSetup(t)
	ann := shop.Shopper("ann@example.com")
	_, guest := api.Post(shop.Store("/carts"), "", apitest.M{})
	token := guest.Get("cart_token").Str()
	api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/items"), shopperLine(product, "Regular", 2), "x-cart-token", token))

	status, cart := api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/merge"), nil,
		"Authorization", "Bearer "+ann, "x-cart-token", token))

	if status != http.StatusOK || cart.Get("id").Str() != guest.Get("cart", "id").Str() {
		t.Errorf("merge: %d %s; guest cart %s", status, cart, guest.Get("cart", "id"))
	}
	if _, current := api.Get(shop.Store("/carts/current"), ann); current.Get("items", 0, "quantity").Num() != 2 {
		t.Errorf("ann's cart %s", current)
	}
}

func TestAVerifiedEmailClaimsItsGuestOrders(t *testing.T) {
	api, shop, product := shopperSetup(t)
	_, guest := api.Post(shop.Store("/carts"), "", apitest.M{})
	token := guest.Get("cart_token").Str()
	api.Send(api.Request(http.MethodPost, shop.Store("/carts/current/items"), shopperLine(product, "Regular", 1), "x-cart-token", token))
	status, body := shopperCheckout(api, shop, "", token, apitest.M{"email": "ann@example.com"})
	api.Want(status, http.StatusCreated, body)

	// Unverified: someone typing Ann's email must not see her orders.
	ann := shop.UnverifiedShopper("ann@example.com")
	if _, orders := api.Get(shop.Store("/orders"), ann); !reflect.DeepEqual(orders.V, []any{}) {
		t.Errorf("unverified sees %s", orders)
	}

	// Once the emailed code proves the address, the account picks up the guest history.
	code := shop.EmailedCode("ann@example.com")
	api.Post(shop.Store("/account/email-verification/confirm"), "", apitest.M{"email": "ann@example.com", "code": code})
	_, orders := api.Get(shop.Store("/orders"), ann)
	if orders.Len() != 1 || orders.Get(0, "email").Str() != "ann@example.com" {
		t.Errorf("verified sees %s", orders)
	}
}

func TestAShopperRetryReturnsTheSameOrder(t *testing.T) {
	api, shop, product := shopperSetup(t)
	ann := shop.Shopper("ann@example.com")
	api.Post(shop.Store("/carts"), ann, apitest.M{})
	api.Post(shop.Store("/carts/current/items"), ann, shopperLine(product, "Regular", 1))
	key := uuid.Must(uuid.NewV7()).String()
	send := func() (int, apitest.J) {
		return api.Send(api.Request(http.MethodPost, shop.Store("/checkout"), apitest.M{},
			"Authorization", "Bearer "+ann, "idempotency-key", key))
	}

	_, first := send()
	status, second := send()

	api.Want(status, http.StatusCreated, second)
	if first.Get("order", "id").Str() != second.Get("order", "id").Str() {
		t.Errorf("orders %s vs %s", first.Get("order", "id"), second.Get("order", "id"))
	}
}

func TestOnlyThisSitesSessionsSignShoppersIn(t *testing.T) {
	api, shop, _ := shopperSetup(t)
	other := api.OpenShop()
	elsewhere := other.Shopper("x@y.co")

	for _, bearer := range []string{shop.Key, elsewhere, "made-up"} {
		if status, _ := api.Get(shop.Store("/orders"), bearer); status != http.StatusUnauthorized {
			t.Errorf("%s: %d", bearer, status)
		}
	}
	if status, _ := api.Get(shop.Store("/orders"), ""); status != http.StatusUnauthorized {
		t.Errorf("no bearer: %d", status)
	}
}
