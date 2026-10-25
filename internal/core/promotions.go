package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
)

// discountJSON is a promotion's discount as stored in promotion.actions:
// {"type": "percent_off", "percent": "10"}, {"type": "amount_off",
// "amount": "5.00"} or {"type": "free_shipping"}.
type discountJSON struct {
	Type    string  `json:"type"`
	Percent *string `json:"percent,omitempty"`
	Amount  *string `json:"amount,omitempty"`
}

// conditionsJSON is stored in promotion.rules.
type conditionsJSON struct {
	MinSubtotal *string     `json:"min_subtotal"`
	ProductIDs  []uuid.UUID `json:"product_ids"`
	CategoryIDs []uuid.UUID `json:"category_ids"`
}

type promotionInput struct {
	discount   discountJSON
	conditions conditionsJSON
	status     string
}

func checkPromotion(body *oapi.PromotionInput, currency string) (promotionInput, error) {
	var in promotionInput
	if strings.TrimSpace(body.Name) == "" {
		return in, unprocessable("name must not be empty")
	}
	raw, _ := body.Discount.MarshalJSON()
	if err := json.Unmarshal(raw, &in.discount); err != nil {
		return in, unprocessable("discount is not valid")
	}
	switch in.discount.Type {
	case "percent_off":
		p, err := amount("percent", in.discount.Percent)
		if err != nil || p == nil || !p.IsPositive() || p.GreaterThan(decimal.NewFromInt(100)) {
			return in, unprocessable("percent must be above 0 and at most 100")
		}
		in.discount = discountJSON{Type: in.discount.Type, Percent: ptr(p.String())}
	case "amount_off":
		a, err := amount("amount", in.discount.Amount)
		if err != nil || a == nil || !a.IsPositive() {
			return in, unprocessable("amount must be above 0")
		}
		if err := checkAmount("amount", *a, currency); err != nil {
			return in, err
		}
		in.discount = discountJSON{Type: in.discount.Type, Amount: ptr(a.String())}
	case "free_shipping":
		in.discount = discountJSON{Type: in.discount.Type}
	default:
		return in, unprocessable("discount type must be percent_off, amount_off or free_shipping")
	}
	in.conditions = conditionsJSON{ProductIDs: []uuid.UUID{}, CategoryIDs: []uuid.UUID{}}
	if c := body.Conditions; c != nil {
		if m, err := amount("min_subtotal", c.MinSubtotal); err != nil {
			return in, err
		} else if m != nil {
			in.conditions.MinSubtotal = ptr(m.String())
		}
		in.conditions.ProductIDs = nonNil(deref(c.ProductIds, nil))
		in.conditions.CategoryIDs = nonNil(deref(c.CategoryIds, nil))
	}
	if body.StartsAt != nil && body.EndsAt != nil && !body.EndsAt.After(*body.StartsAt) {
		return in, unprocessable("ends_at must be after starts_at")
	}
	if (body.UsageLimit != nil && *body.UsageLimit < 1) || (body.PerCustomerLimit != nil && *body.PerCustomerLimit < 1) {
		return in, unprocessable("limits must be at least 1")
	}
	in.status = "active"
	if body.Status != nil {
		in.status = string(*body.Status)
	}
	return in, nil
}

func loadPromotionView(ctx context.Context, q *db.Queries, id uuid.UUID) (oapi.PromotionView, error) {
	p, err := q.PromotionByID(ctx, id)
	if err != nil {
		return oapi.PromotionView{}, err
	}
	view := oapi.PromotionView{Id: p.ID, Name: p.Name, StartsAt: p.StartsAt, EndsAt: p.EndsAt, UsageLimit: p.UsageLimit,
		UsageCount: p.UsageCount, PerCustomerLimit: p.PerCustomerLimit, Status: p.Status}
	if err := view.Discount.UnmarshalJSON(p.Actions); err != nil {
		return view, err
	}
	var c conditionsJSON
	_ = json.Unmarshal(p.Rules, &c)
	view.Conditions = oapi.ConditionsInput{MinSubtotal: c.MinSubtotal, ProductIds: ptr(nonNil(c.ProductIDs)), CategoryIds: ptr(nonNil(c.CategoryIDs))}
	return view, nil
}

func (s *Server) ListPromotions(ctx context.Context, req oapi.ListPromotionsRequestObject) (oapi.ListPromotionsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListPromotions200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		ids, err := q.PromotionIDs(ctx)
		if err != nil {
			return err
		}
		for _, id := range ids {
			v, err := loadPromotionView(ctx, q, id)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreatePromotion creates a promotion. One with no coupon codes applies
// automatically to every qualifying cart; once it has codes, a shopper must
// enter one.
func (s *Server) CreatePromotion(ctx context.Context, req oapi.CreatePromotionRequestObject) (oapi.CreatePromotionResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var view oapi.PromotionView
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		in, err := checkPromotion(req.Body, site.Currency)
		if err != nil {
			return err
		}
		rules, _ := json.Marshal(in.conditions)
		actions, _ := json.Marshal(in.discount)
		id := newID()
		if err := q.InsertPromotion(ctx, db.InsertPromotionParams{
			SiteID: req.SiteId, ID: id, Name: strings.TrimSpace(req.Body.Name), PromotionType: in.discount.Type,
			StartsAt: req.Body.StartsAt, EndsAt: req.Body.EndsAt, UsageLimit: req.Body.UsageLimit,
			PerCustomerLimit: req.Body.PerCustomerLimit, Status: in.status, Rules: rules, Actions: actions,
		}); err != nil {
			return err
		}
		view, err = loadPromotionView(ctx, q, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreatePromotion201JSONResponse(view), nil
}

func (s *Server) GetPromotion(ctx context.Context, req oapi.GetPromotionRequestObject) (oapi.GetPromotionResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var view oapi.PromotionView
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		view, err = loadPromotionView(ctx, q, req.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.GetPromotion200JSONResponse(view), nil
}

// UpdatePromotion replaces a promotion's settings; its usage count is kept.
func (s *Server) UpdatePromotion(ctx context.Context, req oapi.UpdatePromotionRequestObject) (oapi.UpdatePromotionResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var view oapi.PromotionView
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		in, err := checkPromotion(req.Body, site.Currency)
		if err != nil {
			return err
		}
		rules, _ := json.Marshal(in.conditions)
		actions, _ := json.Marshal(in.discount)
		n, err := q.UpdatePromotion(ctx, db.UpdatePromotionParams{
			ID: req.Id, Name: strings.TrimSpace(req.Body.Name), PromotionType: in.discount.Type,
			StartsAt: req.Body.StartsAt, EndsAt: req.Body.EndsAt, UsageLimit: req.Body.UsageLimit,
			PerCustomerLimit: req.Body.PerCustomerLimit, Status: in.status, Rules: rules, Actions: actions,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		view, err = loadPromotionView(ctx, q, req.Id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdatePromotion200JSONResponse(view), nil
}

func couponJSON(id uuid.UUID, code string, maxUses *int32, usage int32, customer *uuid.UUID, status string) oapi.Coupon {
	return oapi.Coupon{Id: id, Code: code, MaxUses: maxUses, UsageCount: usage, CustomerId: customer, Status: status}
}

func (s *Server) ListCoupons(ctx context.Context, req oapi.ListCouponsRequestObject) (oapi.ListCouponsResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	out := oapi.ListCoupons200JSONResponse{}
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		rows, err := q.Coupons(ctx, req.Id)
		for _, c := range rows {
			out = append(out, couponJSON(c.ID, c.Code, c.MaxUses, c.UsageCount, c.CustomerID, c.Status))
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateCoupon adds a code; the promotion then applies only with a code.
func (s *Server) CreateCoupon(ctx context.Context, req oapi.CreateCouponRequestObject) (oapi.CreateCouponResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	code := strings.TrimSpace(req.Body.Code)
	if code == "" || len(code) > 64 || strings.IndexFunc(code, unicode.IsSpace) >= 0 {
		return nil, unprocessable("code must be 1-64 characters without spaces")
	}
	if req.Body.MaxUses != nil && *req.Body.MaxUses < 1 {
		return nil, unprocessable("max_uses must be at least 1")
	}
	var c db.InsertCouponRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		c, err = q.InsertCoupon(ctx, db.InsertCouponParams{SiteID: req.SiteId, ID: newID(), PromotionID: req.Id, Code: code,
			MaxUses: req.Body.MaxUses, CustomerID: req.Body.CustomerId})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateCoupon201JSONResponse(couponJSON(c.ID, c.Code, c.MaxUses, c.UsageCount, c.CustomerID, c.Status)), nil
}

func (s *Server) UpdateCoupon(ctx context.Context, req oapi.UpdateCouponRequestObject) (oapi.UpdateCouponResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	if req.Body.Status != "active" && req.Body.Status != "disabled" {
		return nil, unprocessable("status must be active or disabled")
	}
	var c db.SetCouponStatusRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		c, err = q.SetCouponStatus(ctx, db.SetCouponStatusParams{ID: req.CouponId, PromotionID: req.Id, Status: req.Body.Status})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateCoupon200JSONResponse(couponJSON(c.ID, c.Code, c.MaxUses, c.UsageCount, c.CustomerID, c.Status)), nil
}

// ApplyCoupon puts a coupon on the cart; 422 if it cannot be used now.
func (s *Server) ApplyCoupon(ctx context.Context, req oapi.ApplyCouponRequestObject) (oapi.ApplyCouponResponseObject, error) {
	code := strings.TrimSpace(req.Body.Code)
	key, err := s.cartKey(ctx, req.SiteId, req.Params.XCartToken)
	if err != nil {
		return nil, err
	}
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, func(q *db.Queries, cartID uuid.UUID) error {
		found, err := couponPromotion(ctx, q, code, key.customer())
		if err != nil {
			return err
		}
		if found == nil {
			return unprocessable("that code is not valid right now")
		}
		return q.SetCartCoupon(ctx, db.SetCartCouponParams{ID: cartID, CouponCode: &code})
	})
	if err != nil {
		return nil, err
	}
	return oapi.ApplyCoupon200JSONResponse(cart), nil
}

func (s *Server) RemoveCoupon(ctx context.Context, req oapi.RemoveCouponRequestObject) (oapi.RemoveCouponResponseObject, error) {
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, func(q *db.Queries, cartID uuid.UUID) error {
		return q.SetCartCoupon(ctx, db.SetCartCouponParams{ID: cartID})
	})
	if err != nil {
		return nil, err
	}
	return oapi.RemoveCoupon200JSONResponse(cart), nil
}

// customer is the signed-in shopper's customer id, if any.
func (k cartKey) customer() *uuid.UUID {
	if k.shopper == nil {
		return nil
	}
	return &k.shopper.CustomerID
}

// candidate is a promotion that may apply to a cart, with what checkout
// needs to redeem it.
type candidate struct {
	promotion commerce.Promotion
	name      string
	couponID  *uuid.UUID
	code      string
}

func toPromotion(id uuid.UUID, actions, rules json.RawMessage) (commerce.Promotion, bool) {
	var d discountJSON
	if json.Unmarshal(actions, &d) != nil {
		return commerce.Promotion{}, false
	}
	p := commerce.Promotion{ID: id}
	switch d.Type {
	case "percent_off":
		v, err := amount("percent", d.Percent)
		if err != nil || v == nil {
			return p, false
		}
		p.Discount = commerce.Discount{Kind: commerce.PercentOff, Value: *v}
	case "amount_off":
		v, err := amount("amount", d.Amount)
		if err != nil || v == nil {
			return p, false
		}
		p.Discount = commerce.Discount{Kind: commerce.AmountOff, Value: *v}
	case "free_shipping":
		p.Discount = commerce.Discount{Kind: commerce.FreeShipping}
	default:
		return p, false
	}
	var c conditionsJSON
	_ = json.Unmarshal(rules, &c)
	p.Conditions = commerce.Conditions{ProductIDs: c.ProductIDs, CategoryIDs: c.CategoryIDs}
	if m, err := amount("min_subtotal", c.MinSubtotal); err == nil {
		p.Conditions.MinSubtotal = m
	}
	return p, true
}

// usedUp: the customer already redeemed the promotion as often as allowed.
func usedUp(ctx context.Context, q *db.Queries, promotionID uuid.UUID, limit *int32, customer *uuid.UUID) (bool, error) {
	if limit == nil || customer == nil {
		return false, nil
	}
	n, err := q.Redemptions(ctx, db.RedemptionsParams{PromotionID: promotionID, CustomerID: customer})
	return n >= int64(*limit), err
}

// couponPromotion is the coupon's promotion if the code can be used now by
// customer, else nil.
func couponPromotion(ctx context.Context, q *db.Queries, code string, customer *uuid.UUID) (*candidate, error) {
	row, err := q.CouponPromotion(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.CouponCustomer != nil && (customer == nil || *row.CouponCustomer != *customer) {
		return nil, nil
	}
	if spent, err := usedUp(ctx, q, row.ID, row.PerCustomerLimit, customer); err != nil || spent {
		return nil, err
	}
	p, ok := toPromotion(row.ID, row.Actions, row.Rules)
	if !ok {
		return nil, nil
	}
	return &candidate{promotion: p, name: row.Name, couponID: &row.CouponID, code: row.Code}, nil
}

// promotionCandidates are the automatic promotions in force, plus the
// cart's coupon promotion if usable. Per-customer limits are checked when
// the customer is known.
func promotionCandidates(ctx context.Context, q *db.Queries, coupon *string, customer *uuid.UUID) ([]candidate, error) {
	rows, err := q.AutomaticPromotions(ctx)
	if err != nil {
		return nil, err
	}
	var found []candidate
	for _, r := range rows {
		if spent, err := usedUp(ctx, q, r.ID, r.PerCustomerLimit, customer); err != nil {
			return nil, err
		} else if spent {
			continue
		}
		if p, ok := toPromotion(r.ID, r.Actions, r.Rules); ok {
			found = append(found, candidate{promotion: p, name: r.Name})
		}
	}
	if coupon != nil {
		c, err := couponPromotion(ctx, q, *coupon, customer)
		if err != nil {
			return nil, err
		}
		if c != nil {
			found = append(found, *c)
		}
	}
	return found, nil
}

// categoriesOf is the category ids per product, for category-limited
// promotions.
func categoriesOf(ctx context.Context, q *db.Queries, productIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	rows, err := q.CategoriesOfProducts(ctx, productIDs)
	out := map[uuid.UUID][]uuid.UUID{}
	for _, r := range rows {
		out[r.ProductID] = append(out[r.ProductID], r.CategoryID)
	}
	return out, err
}

// redeemed is an applied promotion and the coupon that unlocked it.
type redeemed struct {
	promotionID uuid.UUID
	couponID    *uuid.UUID
}

// redeemPromotions counts a use of each applied promotion (and coupon) for
// an order, failing with 409 if one ran out since the quote.
func redeemPromotions(ctx context.Context, q *db.Queries, siteID, orderID, customer uuid.UUID, applied []redeemed) error {
	for _, a := range applied {
		n, err := q.CountPromotionUse(ctx, a.promotionID)
		if err != nil {
			return err
		}
		if n == 0 {
			return conflict("a promotion ran out while checking out; review the cart")
		}
		if a.couponID != nil {
			n, err := q.CountCouponUse(ctx, *a.couponID)
			if err != nil {
				return err
			}
			if n == 0 {
				return conflict("the coupon was used up while checking out")
			}
		}
		if err := q.InsertRedemption(ctx, db.InsertRedemptionParams{
			SiteID: siteID, ID: newID(), PromotionID: a.promotionID, CustomerID: &customer, CouponCodeID: a.couponID, OrderID: orderID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// releasePromotions gives back a cancelled order's promotion and coupon uses.
func releasePromotions(ctx context.Context, q *db.Queries, orderID uuid.UUID) error {
	released, err := q.DeleteRedemptions(ctx, orderID)
	if err != nil {
		return err
	}
	for _, r := range released {
		if err := q.UncountPromotionUse(ctx, r.PromotionID); err != nil {
			return err
		}
		if r.CouponCodeID != nil {
			if err := q.UncountCouponUse(ctx, *r.CouponCodeID); err != nil {
				return err
			}
		}
	}
	return nil
}
