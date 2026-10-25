package core

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
)

func checkAddress(a *oapi.Address) error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(a.Line1) == "" {
		return unprocessable("address line1 must not be empty")
	}
	if !isUpperCode(a.CountryCode, 2) {
		return unprocessable("country_code must be an ISO 3166-1 alpha-2 code such as AU")
	}
	return nil
}

type appliedDiscount struct {
	PromotionID uuid.UUID `json:"promotion_id"`
	Name        string    `json:"name"`
	// CouponCode is the coupon that unlocked it; nil for automatic ones.
	CouponCode   *string         `json:"coupon_code"`
	Amount       decimal.Decimal `json:"amount"`
	FreeShipping bool            `json:"free_shipping"`
}

// priced is a quote plus what checkout needs to place and redeem it.
type priced struct {
	currency         string
	pricesIncludeTax bool
	discounts        []appliedDiscount
	options          []shippingOption
	totals           commerce.Totals
	// lineDiscounts is per priced line (cart lines without a problem), in
	// cart order.
	lineDiscounts []decimal.Decimal
	method        *shippingOption
	redemptions   []redeemed
	// couponApplied: the cart's coupon is among the applied discounts.
	couponApplied bool
}

func (p priced) quote() oapi.Quote {
	q := oapi.Quote{
		Currency: p.currency, Subtotal: money(p.totals.Subtotal, p.currency), Discounts: []oapi.AppliedDiscount{},
		DiscountTotal: money(p.totals.DiscountTotal, p.currency), ShippingOptions: []oapi.ShippingOption{},
		ShippingTotal: money(p.totals.ShippingTotal, p.currency), ShippingTax: money(p.totals.ShippingTax, p.currency),
		TaxTotal: money(p.totals.TaxTotal, p.currency), GrandTotal: money(p.totals.GrandTotal, p.currency),
		PricesIncludeTax: p.pricesIncludeTax,
	}
	for _, d := range p.discounts {
		q.Discounts = append(q.Discounts, oapi.AppliedDiscount{PromotionId: d.PromotionID, Name: d.Name, CouponCode: d.CouponCode,
			Amount: money(d.Amount, p.currency), FreeShipping: d.FreeShipping})
	}
	for _, o := range p.options {
		q.ShippingOptions = append(q.ShippingOptions, o.json(p.currency))
	}
	if p.method != nil {
		q.ShippingMethodCode = &p.method.Code
	}
	return q
}

// discountSnapshot is what the order records about its discounts.
func (p priced) discountSnapshot() json.RawMessage {
	discounts := []map[string]any{}
	for _, d := range p.discounts {
		discounts = append(discounts, map[string]any{"promotion_id": d.PromotionID, "name": d.Name, "coupon_code": d.CouponCode,
			"amount": money(d.Amount, p.currency), "free_shipping": d.FreeShipping})
	}
	b, _ := json.Marshal(map[string]any{"discounts": discounts})
	return b
}

type destination struct {
	shipping, billing *oapi.Address
	methodCode        *string
}

// taxPlace is where tax applies: the business for pickup; else the
// shipping address, the billing address, or the business.
func taxPlace(site db.GetSiteRow, dest destination, method *shippingOption) *commerce.Place {
	var business *commerce.Place
	if site.CountryCode != nil {
		business = &commerce.Place{Country: *site.CountryCode, Region: site.RegionCode}
	}
	if method != nil && method.Kind == "pickup" {
		return business
	}
	for _, a := range []*oapi.Address{dest.shipping, dest.billing} {
		if a != nil {
			return &commerce.Place{Country: a.CountryCode, Region: a.State}
		}
	}
	return business
}

// priceOrder computes discounts, shipping and tax for a cart going to a
// destination. Checkout uses the same computation, so a quote is what you
// pay. customer (when known) lets per-customer promotion limits apply.
func priceOrder(ctx context.Context, q *db.Queries, site db.GetSiteRow, cart pricedCart, dest destination, customer *uuid.UUID) (priced, error) {
	p := priced{currency: site.Currency, pricesIncludeTax: site.PricesIncludeTax}
	var err error
	if p.options, err = shippingOptions(ctx, q, dest.shipping, cart.Subtotal, site.Currency); err != nil {
		return p, err
	}
	if dest.methodCode != nil {
		for i := range p.options {
			if p.options[i].Code == *dest.methodCode {
				p.method = &p.options[i]
			}
		}
		if p.method == nil {
			return p, unprocessable("shipping method %s is not available for this order", *dest.methodCode)
		}
	}
	var lines []pricedLine
	for _, l := range cart.Items {
		if l.Problem == nil {
			lines = append(lines, l)
		}
	}
	candidates, err := promotionCandidates(ctx, q, cart.CouponCode, customer)
	if err != nil {
		return p, err
	}
	productIDs := make([]uuid.UUID, len(lines))
	for i, l := range lines {
		productIDs[i] = l.ProductID
	}
	categories, err := categoriesOf(ctx, q, productIDs)
	if err != nil {
		return p, err
	}
	promoLines := make([]commerce.PromotionLine, len(lines))
	for i, l := range lines {
		promoLines[i] = commerce.PromotionLine{ProductID: l.ProductID, CategoryIDs: categories[l.ProductID], Amount: l.LineTotal}
	}
	promotions := make([]commerce.Promotion, len(candidates))
	for i, c := range candidates {
		promotions[i] = c.promotion
	}
	outcome := commerce.ApplyPromotions(promotions, promoLines, site.Currency)
	for _, a := range outcome.Applied {
		for _, c := range candidates {
			if c.promotion.ID != a.PromotionID {
				continue
			}
			d := appliedDiscount{PromotionID: a.PromotionID, Name: c.name, Amount: a.Amount, FreeShipping: a.FreeShipping}
			if c.couponID != nil {
				d.CouponCode = ptr(c.code)
				p.couponApplied = true
			}
			p.discounts = append(p.discounts, d)
			p.redemptions = append(p.redemptions, redeemed{promotionID: a.PromotionID, couponID: c.couponID})
		}
	}
	p.lineDiscounts = outcome.LineDiscounts

	rates, err := loadRatesInForce(ctx, q)
	if err != nil {
		return p, err
	}
	place := taxPlace(site, dest, p.method)
	rateFor := func(classRates []commerce.TaxRate) decimal.Decimal {
		if place == nil {
			return decimal.Zero
		}
		return commerce.EffectiveRate(classRates, *place)
	}
	totalsLines := make([]commerce.TotalsLine, len(lines))
	for i, l := range lines {
		totalsLines[i] = commerce.TotalsLine{Amount: l.LineTotal, Discount: outcome.LineDiscounts[i], TaxRate: rateFor(rates.forProduct(l.TaxClassID))}
	}
	mode := commerce.Exclusive
	if site.PricesIncludeTax {
		mode = commerce.Inclusive
	}
	shippingPrice, shippingRate := decimal.Zero, decimal.Zero
	if p.method != nil {
		if !outcome.FreeShipping {
			shippingPrice = p.method.Price
		}
		shippingRate = rateFor(rates.forClass(p.method.TaxClassID))
	}
	p.totals = commerce.ComputeTotals(totalsLines, shippingPrice, shippingRate, mode, site.Currency)
	return p, nil
}

// QuoteCart is the discounts, shipping options and totals for the current
// cart and an address. Call again with shipping_method_code once one is
// picked.
func (s *Server) QuoteCart(ctx context.Context, req oapi.QuoteCartRequestObject) (oapi.QuoteCartResponseObject, error) {
	for _, a := range []*oapi.Address{req.Body.ShippingAddress, req.Body.BillingAddress} {
		if err := checkAddress(a); err != nil {
			return nil, err
		}
	}
	key, err := s.cartKey(ctx, req.SiteId, req.Params.XCartToken)
	if err != nil {
		return nil, err
	}
	var out priced
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		cartID, err := resolveCart(ctx, q, key)
		if err != nil {
			return err
		}
		cart, err := priceCart(ctx, q, cartID, site.Currency)
		if err != nil {
			return err
		}
		out, err = priceOrder(ctx, q, site, cart, destination{shipping: req.Body.ShippingAddress, billing: req.Body.BillingAddress, methodCode: req.Body.ShippingMethodCode}, key.customer())
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.QuoteCart200JSONResponse(out.quote()), nil
}
