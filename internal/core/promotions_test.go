package core_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

type promoWorld struct {
	t       *testing.T
	api     *apitest.API
	shop    apitest.Shop
	product apitest.J
}

// promoWho is who is shopping: a guest cart token, or a shopper bearer.
type promoWho struct {
	cartToken string
	bearer    string
}

func newPromoWorld(t *testing.T) *promoWorld {
	t.Helper()
	api := apitest.New(t)
	shop := api.OpenShop()
	return &promoWorld{t: t, api: api, shop: shop, product: shop.PadThai()}
}

func (w *promoWorld) promotion(body apitest.M) apitest.J {
	w.t.Helper()
	status, promotion := w.api.Post(w.shop.Admin("/promotions"), w.shop.Key, body)
	w.api.Want(status, http.StatusCreated, promotion)
	return promotion
}

func (w *promoWorld) coupon(promotion apitest.J, body apitest.M) apitest.J {
	w.t.Helper()
	uri := w.shop.Admin(fmt.Sprintf("/promotions/%s/coupons", promotion.Get("id").Str()))
	status, coupon := w.api.Post(uri, w.shop.Key, body)
	w.api.Want(status, http.StatusCreated, coupon)
	return coupon
}

func (w *promoWorld) req(method, path string, who promoWho, body any, headers ...string) *http.Request {
	if who.bearer != "" {
		headers = append(headers, "Authorization", "Bearer "+who.bearer)
	} else {
		headers = append(headers, "x-cart-token", who.cartToken)
	}
	return w.api.Request(method, w.shop.Store(path), body, headers...)
}

// cart holds count Large Spicy Pad Thai (21.00 each).
func (w *promoWorld) cart(count int, shopperToken string) promoWho {
	w.t.Helper()
	var who promoWho
	if shopperToken != "" {
		w.api.Post(w.shop.Store("/carts"), shopperToken, apitest.M{})
		who = promoWho{bearer: shopperToken}
	} else {
		_, cart := w.api.Post(w.shop.Store("/carts"), "", apitest.M{})
		who = promoWho{cartToken: cart.Get("cart_token").Str()}
	}
	line := apitest.M{"variant_id": apitest.VariantID(w.product, "Large"), "quantity": count,
		"option_value_ids": []string{apitest.OptionID(w.product, "spice", "spicy")}}
	status, body := w.api.Send(w.req(http.MethodPost, "/carts/current/items", who, line))
	w.api.Want(status, http.StatusOK, body)
	return who
}

func (w *promoWorld) apply(who promoWho, code string) (int, apitest.J) {
	return w.api.Send(w.req(http.MethodPut, "/carts/current/coupon", who, apitest.M{"code": code}))
}

func (w *promoWorld) quote(who promoWho, body apitest.M) apitest.J {
	w.t.Helper()
	status, quote := w.api.Send(w.req(http.MethodPost, "/carts/current/quote", who, body))
	w.api.Want(status, http.StatusOK, quote)
	return quote
}

func (w *promoWorld) checkout(who promoWho) (int, apitest.J) {
	return w.api.Send(w.req(http.MethodPost, "/checkout", who, apitest.M{"email": "ann@example.com"},
		"idempotency-key", uuid.NewString()))
}

func (w *promoWorld) usage(promotion apitest.J) int {
	_, body := w.api.Get(w.shop.Admin(fmt.Sprintf("/promotions/%s", promotion.Get("id").Str())), w.shop.Key)
	return int(body.Get("usage_count").Num())
}

func promoJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAnAutomaticPromotionDiscountsEveryQualifyingCart(t *testing.T) {
	w := newPromoWorld(t)
	promotion := w.promotion(apitest.M{"name": "Launch week", "discount": apitest.M{"type": "percent_off", "percent": "10"}})
	who := w.cart(2, "")

	quote := w.quote(who, apitest.M{})
	// coupon_code is omitted rather than null.
	want := promoJSON(t, []apitest.M{{"promotion_id": promotion.Get("id").V, "name": "Launch week",
		"amount": "4.20", "free_shipping": false}})
	if !reflect.DeepEqual(quote.Get("discounts").V, want) {
		t.Errorf("discounts: %s", quote.Get("discounts"))
	}
	if quote.Get("discount_total").Str() != "4.20" || quote.Get("grand_total").Str() != "37.80" {
		t.Errorf("quote: %s", quote)
	}

	status, placed := w.checkout(who)
	w.api.Want(status, http.StatusCreated, placed)
	order := placed.Get("order")
	if order.Get("discount_total").Str() != "4.20" || order.Get("grand_total").Str() != "37.80" {
		t.Errorf("order: %s", order)
	}
	if order.Get("items", 0, "discount_total").Str() != "4.20" || order.Get("items", 0, "total").Str() != "37.80" {
		t.Errorf("item: %s", order.Get("items", 0))
	}
	if got := w.usage(promotion); got != 1 {
		t.Errorf("usage: %d", got)
	}
}

func TestASingleUseCouponWorksOnce(t *testing.T) {
	w := newPromoWorld(t)
	promotion := w.promotion(apitest.M{"name": "Five off", "discount": apitest.M{"type": "amount_off", "amount": "5"}})
	w.coupon(promotion, apitest.M{"code": "WELCOME5", "max_uses": 1})
	first := w.cart(1, "")

	if status, _ := w.apply(first, "NOPE"); status != http.StatusUnprocessableEntity {
		t.Errorf("unknown code: %d", status)
	}
	status, cart := w.apply(first, "welcome5")
	if status != http.StatusOK || cart.Get("coupon_code").Str() != "welcome5" {
		t.Errorf("codes are case-insensitive: %d %s", status, cart)
	}
	if q := w.quote(first, apitest.M{}); q.Get("discounts", 0, "coupon_code").Str() != "WELCOME5" {
		t.Errorf("quote: %s", q)
	}
	status, placed := w.checkout(first)
	if status != http.StatusCreated || placed.Get("order", "grand_total").Str() != "16.00" {
		t.Errorf("checkout: %d %s", status, placed)
	}

	second := w.cart(1, "")
	if status, _ := w.apply(second, "WELCOME5"); status != http.StatusUnprocessableEntity {
		t.Errorf("a used-up code: %d", status)
	}
}

func TestConditionsDecideWhetherAPromotionApplies(t *testing.T) {
	w := newPromoWorld(t)
	_, drinks := w.api.Post(w.shop.Admin("/categories"), w.shop.Key, apitest.M{"slug": "drinks", "name": "Drinks"})
	w.promotion(apitest.M{"name": "Big order", "discount": apitest.M{"type": "percent_off", "percent": "20"},
		"conditions": apitest.M{"min_subtotal": "50"}})
	w.promotion(apitest.M{"name": "Drinks deal", "discount": apitest.M{"type": "amount_off", "amount": "3"},
		"conditions": apitest.M{"category_ids": []any{drinks.Get("id").V}}})

	small := w.cart(2, "")
	if q := w.quote(small, apitest.M{}); !reflect.DeepEqual(q.Get("discounts").V, []any{}) {
		t.Errorf("42.00 is under 50 and has no drinks: %s", q.Get("discounts"))
	}
	big := w.cart(3, "")
	quote := w.quote(big, apitest.M{})
	if quote.Get("discounts", 0, "name").Str() != "Big order" || quote.Get("discount_total").Str() != "12.60" {
		t.Errorf("quote: %s", quote)
	}
}

func TestFreeShippingZeroesTheChosenMethod(t *testing.T) {
	w := newPromoWorld(t)
	_, zone := w.api.Post(w.shop.Admin("/shipping-zones"), w.shop.Key,
		apitest.M{"code": "us", "name": "US", "regions": []apitest.M{{"country_code": "US"}}})
	_, method := w.api.Post(w.shop.Admin("/shipping-methods"), w.shop.Key,
		apitest.M{"code": "post", "name": "Post", "kind": "delivery"})
	w.api.Post(w.shop.Admin("/shipping-rates"), w.shop.Key,
		apitest.M{"method_id": method.Get("id").V, "zone_id": zone.Get("id").V, "price": "9"})
	promotion := w.promotion(apitest.M{"name": "Free post", "discount": apitest.M{"type": "free_shipping"}})
	w.coupon(promotion, apitest.M{"code": "SHIPFREE"})
	who := w.cart(1, "")
	destination := apitest.M{"shipping_address": apitest.M{"line1": "1 Main St", "country_code": "US"}, "shipping_method_code": "post"}

	if q := w.quote(who, destination); q.Get("shipping_total").Str() != "9.00" {
		t.Errorf("before the coupon: %s", q)
	}
	w.apply(who, "SHIPFREE")
	quote := w.quote(who, destination)
	if quote.Get("shipping_total").Str() != "0.00" || quote.Get("grand_total").Str() != "21.00" {
		t.Errorf("quote: %s", quote)
	}
	if quote.Get("shipping_options", 0, "price").Str() != "9.00" {
		t.Errorf("options keep their list price: %s", quote.Get("shipping_options"))
	}
}

func TestPerCustomerLimitsAndTargetedCodes(t *testing.T) {
	w := newPromoWorld(t)
	ann := w.shop.Shopper("ann@example.com")
	bob := w.shop.Shopper("bob@example.com")
	w.promotion(apitest.M{"name": "First order", "discount": apitest.M{"type": "percent_off", "percent": "50"}, "per_customer_limit": 1})

	first := w.cart(1, ann)
	if _, placed := w.checkout(first); placed.Get("order", "discount_total").Str() != "10.50" {
		t.Errorf("first order: %s", placed)
	}
	again := w.cart(1, ann)
	if q := w.quote(again, apitest.M{}); !reflect.DeepEqual(q.Get("discounts").V, []any{}) {
		t.Errorf("Ann has used hers: %s", q.Get("discounts"))
	}
	bobs := w.cart(1, bob)
	if q := w.quote(bobs, apitest.M{}); q.Get("discount_total").Str() != "10.50" {
		t.Errorf("Bob's quote: %s", q)
	}

	// A code targeted at Ann's customer profile is Ann's alone.
	_, orders := w.api.Get(w.shop.Admin("/orders"), w.shop.Key)
	_, annOrder := w.api.Get(w.shop.Admin(fmt.Sprintf("/orders/%s", orders.Get("items", 0, "id").Str())), w.shop.Key)
	annCustomer := annOrder.Get("customer_id").V
	vip := w.promotion(apitest.M{"name": "VIP", "discount": apitest.M{"type": "amount_off", "amount": "1"}})
	w.coupon(vip, apitest.M{"code": "ANNONLY", "customer_id": annCustomer})
	if status, _ := w.apply(bobs, "ANNONLY"); status != http.StatusUnprocessableEntity {
		t.Errorf("Bob applied Ann's code: %d", status)
	}
	if status, b := w.apply(again, "ANNONLY"); status != http.StatusOK {
		t.Errorf("Ann applies her code: %d %s", status, b)
	}
}

func TestCancellingGivesTheCouponUseBack(t *testing.T) {
	w := newPromoWorld(t)
	promotion := w.promotion(apitest.M{"name": "Once", "discount": apitest.M{"type": "amount_off", "amount": "2"}})
	w.coupon(promotion, apitest.M{"code": "ONCE", "max_uses": 1})
	who := w.cart(1, "")
	w.apply(who, "ONCE")
	_, placed := w.checkout(who)
	if got := w.usage(promotion); got != 1 {
		t.Errorf("usage after checkout: %d", got)
	}

	cancel := w.shop.Admin(fmt.Sprintf("/orders/%s/cancel", placed.Get("order", "id").Str()))
	if status, b := w.api.Post(cancel, w.shop.Key, apitest.M{}); status != http.StatusOK {
		t.Errorf("cancel: %d %s", status, b)
	}

	if got := w.usage(promotion); got != 0 {
		t.Errorf("usage after cancelling: %d", got)
	}
	next := w.cart(1, "")
	if status, b := w.apply(next, "ONCE"); status != http.StatusOK {
		t.Errorf("reapply: %d %s", status, b)
	}
}

func TestAOneUsePromotionIsRedeemedAtMostOnceUnderContention(t *testing.T) {
	w := newPromoWorld(t)
	promotion := w.promotion(apitest.M{"name": "First buyer", "discount": apitest.M{"type": "amount_off", "amount": "5"}, "usage_limit": 1})
	carts := []promoWho{w.cart(1, ""), w.cart(1, "")}

	statuses := make([]int, 2)
	bodies := make([]apitest.J, 2)
	var wg sync.WaitGroup
	for i, who := range carts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i], bodies[i] = w.checkout(who)
		}()
	}
	wg.Wait()

	discounted := 0
	for i := range statuses {
		if statuses[i] == http.StatusCreated && bodies[i].Get("order", "discount_total").Str() == "5.00" {
			discounted++
		}
		if statuses[i] != http.StatusCreated && statuses[i] != http.StatusConflict {
			t.Errorf("checkout %d: %d %s", i, statuses[i], bodies[i])
		}
	}
	if discounted != 1 {
		t.Errorf("discounted %d orders: %s %s", discounted, bodies[0], bodies[1])
	}
	if got := w.usage(promotion); got != 1 {
		t.Errorf("usage: %d", got)
	}
}

func TestPromotionsAreValidated(t *testing.T) {
	w := newPromoWorld(t)
	for _, body := range []apitest.M{
		{"name": "x", "discount": apitest.M{"type": "percent_off", "percent": "150"}},
		{"name": "x", "discount": apitest.M{"type": "amount_off", "amount": "0.001"}},
		{"name": " ", "discount": apitest.M{"type": "free_shipping"}},
		{"name": "x", "discount": apitest.M{"type": "free_shipping"}, "usage_limit": 0},
	} {
		if status, problem := w.api.Post(w.shop.Admin("/promotions"), w.shop.Key, body); status != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d %s", body, status, problem)
		}
	}
}
