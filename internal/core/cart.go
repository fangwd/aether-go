package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"aether/internal/commerce"
	"aether/internal/core/oapi"
	"aether/internal/db"
)

const maxQuantity = 999

// newToken is 256 random bits, hex-encoded: random to the caller, only its
// sha256 at rest.
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// cartKey is who is asking for a cart: a signed-in shopper (bearer) or a
// guest (X-Cart-Token).
type cartKey struct {
	siteID    uuid.UUID
	shopper   *Shopper
	guestHash []byte
}

// fingerprint identifies who is checking out, independent of which cart,
// so an idempotent retry still matches after the cart is converted.
func (k cartKey) fingerprint() string {
	if k.shopper != nil {
		return fmt.Sprintf("checkout:%s", k.shopper.CustomerID)
	}
	return "checkout:guest:" + hex.EncodeToString(k.guestHash)
}

func (s *Server) cartKey(ctx context.Context, siteID uuid.UUID, cartToken *string) (cartKey, error) {
	sh, err := s.shopper(ctx, siteID)
	if err != nil {
		return cartKey{}, err
	}
	if sh != nil {
		return cartKey{siteID: siteID, shopper: sh}, nil
	}
	if cartToken == nil || *cartToken == "" {
		return cartKey{}, errNotFound
	}
	return cartKey{siteID: siteID, guestHash: hashToken(*cartToken)}, nil
}

// resolveCart is the active cart the key refers to.
func resolveCart(ctx context.Context, q *db.Queries, key cartKey) (uuid.UUID, error) {
	var id uuid.UUID
	var err error
	if key.shopper != nil {
		id, err = q.CustomerCart(ctx, &key.shopper.CustomerID)
	} else {
		id, err = q.GuestCart(ctx, key.guestHash)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return id, errNotFound
	}
	return id, err
}

// resolveCartForCheckout is like resolveCart, but tells a spent guest cart
// (409) from a missing one.
func resolveCartForCheckout(ctx context.Context, q *db.Queries, key cartKey) (uuid.UUID, error) {
	if key.shopper == nil {
		if status, err := q.GuestCartStatus(ctx, key.guestHash); err == nil && status != "active" {
			return uuid.Nil, conflict("this cart has already been checked out")
		}
	}
	return resolveCart(ctx, q, key)
}

func checkQuantity(quantity int32) error {
	if quantity < 1 || quantity > maxQuantity {
		return unprocessable("quantity must be between 1 and %d", maxQuantity)
	}
	return nil
}

// chosenOption is what an add-on value shows and costs.
type chosenOption struct {
	Group      string
	ValueID    uuid.UUID
	Label      string
	PriceExtra decimal.Decimal
}

// productOptions are a product's add-ons: selection rules plus what each
// value shows and costs.
type productOptions struct {
	groups []commerce.OptionGroup
	values map[uuid.UUID]chosenOption
}

// chosen is the chosen values in group order, for display and snapshots.
func (p productOptions) chosen(ids []uuid.UUID) []chosenOption {
	var out []chosenOption
	for _, g := range p.groups {
		for _, v := range g.Values {
			if slices.Contains(ids, v) {
				out = append(out, p.values[v])
			}
		}
	}
	return out
}

func loadOptions(ctx context.Context, q *db.Queries, productIDs []uuid.UUID) (map[uuid.UUID]productOptions, error) {
	rows, err := q.ProductOptions(ctx, productIDs)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]productOptions{}
	for _, r := range rows {
		po := out[r.ProductID]
		if po.values == nil {
			po.values = map[uuid.UUID]chosenOption{}
		}
		if len(po.groups) == 0 || po.groups[len(po.groups)-1].ID != r.GroupID {
			var maxSel *int
			if r.MaxSelect != nil {
				m := int(*r.MaxSelect)
				maxSel = &m
			}
			po.groups = append(po.groups, commerce.OptionGroup{ID: r.GroupID, Name: r.GroupName, MinSelect: int(r.MinSelect), MaxSelect: maxSel})
		}
		last := &po.groups[len(po.groups)-1]
		last.Values = append(last.Values, r.ValueID)
		po.values[r.ValueID] = chosenOption{Group: r.GroupName, ValueID: r.ValueID, Label: r.Label, PriceExtra: r.PriceExtra}
		out[r.ProductID] = po
	}
	return out, nil
}

// checkPurchasable checks a variant can be bought with these options and
// returns its unit price.
func checkPurchasable(ctx context.Context, q *db.Queries, variantID uuid.UUID, options []uuid.UUID) (decimal.Decimal, error) {
	v, err := q.PurchasableVariant(ctx, variantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, unprocessable("that product is not available")
	}
	if err != nil {
		return decimal.Zero, err
	}
	all, err := loadOptions(ctx, q, []uuid.UUID{v.ProductID})
	if err != nil {
		return decimal.Zero, err
	}
	po := all[v.ProductID]
	if err := commerce.CheckSelection(po.groups, options); err != nil {
		return decimal.Zero, unprocessable("%s", err)
	}
	var extras []decimal.Decimal
	for _, id := range options {
		extras = append(extras, po.values[id].PriceExtra)
	}
	return commerce.UnitPrice(v.Price, v.PriceExtra, extras...), nil
}

// checkCartStock rejects a cart holding more of a stock-tracked variant
// than is available.
func checkCartStock(ctx context.Context, q *db.Queries, variantID, cartID uuid.UUID) error {
	row, err := q.CartStock(ctx, db.CartStockParams{CartID: cartID, VariantID: variantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if row.Wanted > row.Available {
		return unprocessable("only %d available", max(row.Available, 0))
	}
	return nil
}

// pricedLine is one cart line priced from the live catalog.
type pricedLine struct {
	ID, ProductID, VariantID uuid.UUID
	ProductSlug, ProductName string
	VariantName              *string
	Attributes               map[string]any
	Options                  []chosenOption
	OptionValueIDs           []uuid.UUID
	Quantity                 int32
	UnitPrice, LineTotal     decimal.Decimal
	// Problem is why this line cannot be bought as it stands.
	Problem    *string
	TaxClassID *uuid.UUID
	Sku        *string
}

type pricedCart struct {
	ID         uuid.UUID
	Currency   string
	CouponCode *string
	Items      []pricedLine
	// Subtotal is the sum of lines without a problem.
	Subtotal  decimal.Decimal
	ItemCount int64
	// Ready is true when the cart has items and none has a problem.
	Ready bool
}

// priceCart prices a cart from the live catalog.
func priceCart(ctx context.Context, q *db.Queries, cartID uuid.UUID, currency string) (pricedCart, error) {
	cart := pricedCart{ID: cartID, Currency: currency}
	rows, err := q.PriceCartLines(ctx, cartID)
	if err != nil {
		return cart, err
	}
	productIDs := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		productIDs[i] = r.ProductID
	}
	options, err := loadOptions(ctx, q, productIDs)
	if err != nil {
		return cart, err
	}
	for _, r := range rows {
		po := options[r.ProductID]
		chosen := po.chosen(r.OptionValueIds)
		var problem *string
		switch {
		case r.ProductStatus != "active" || r.VariantStatus != "active":
			problem = ptr("no longer available")
		case commerce.CheckSelection(po.groups, r.OptionValueIds) != nil:
			problem = ptr("options changed: " + commerce.CheckSelection(po.groups, r.OptionValueIds).Error())
		case r.Available != nil && *r.Available <= 0:
			problem = ptr("sold out")
		case r.Available != nil && r.VariantQuantity > *r.Available:
			problem = ptr(fmt.Sprintf("only %d available", *r.Available))
		}
		var extras []decimal.Decimal
		for _, o := range chosen {
			extras = append(extras, o.PriceExtra)
		}
		unit := commerce.Round(commerce.UnitPrice(r.Price, r.PriceExtra, extras...), currency)
		total := commerce.Round(unit.Mul(decimal.NewFromInt32(r.Quantity)), currency)
		if problem == nil {
			cart.Subtotal = cart.Subtotal.Add(total)
		}
		cart.ItemCount += int64(r.Quantity)
		cart.Items = append(cart.Items, pricedLine{
			ID: r.ID, ProductID: r.ProductID, VariantID: r.VariantID, ProductSlug: r.Slug, ProductName: r.ProductName,
			VariantName: r.VariantName, Attributes: jsonObject(r.Attributes), Options: chosen,
			OptionValueIDs: r.OptionValueIds, Quantity: r.Quantity, UnitPrice: unit, LineTotal: total,
			Problem: problem, TaxClassID: r.TaxClassID,
		})
	}
	cart.Ready = len(cart.Items) > 0
	for _, l := range cart.Items {
		if l.Problem != nil {
			cart.Ready = false
		}
	}
	cart.Subtotal = commerce.Round(cart.Subtotal, currency)
	cart.CouponCode, err = q.CartCoupon(ctx, cartID)
	return cart, err
}

func ptr[T any](v T) *T { return &v }

func (c pricedCart) json() oapi.Cart {
	out := oapi.Cart{Id: c.ID, Currency: c.Currency, CouponCode: c.CouponCode, Items: []oapi.CartLine{},
		Subtotal: money(c.Subtotal, c.Currency), ItemCount: c.ItemCount, Ready: c.Ready}
	for _, l := range c.Items {
		opts := []oapi.ChosenOption{}
		for _, o := range l.Options {
			opts = append(opts, oapi.ChosenOption{Group: o.Group, ValueId: o.ValueID, Label: o.Label, PriceExtra: money(o.PriceExtra, c.Currency)})
		}
		out.Items = append(out.Items, oapi.CartLine{
			Id: l.ID, ProductId: l.ProductID, ProductSlug: l.ProductSlug, ProductName: l.ProductName,
			VariantId: l.VariantID, VariantName: l.VariantName, Attributes: l.Attributes, Options: opts,
			Quantity: l.Quantity, UnitPrice: money(l.UnitPrice, c.Currency), LineTotal: money(l.LineTotal, c.Currency),
			Problem: l.Problem,
		})
	}
	return out
}

// CreateCart starts a cart. Guests get a cart_token; a signed-in shopper
// gets their existing active cart, or a new one tied to their account.
func (s *Server) CreateCart(ctx context.Context, req oapi.CreateCartRequestObject) (oapi.CreateCartResponseObject, error) {
	sh, err := s.shopper(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	var cart pricedCart
	var token *string
	existing := false
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		var id uuid.UUID
		if sh != nil {
			id, err = q.CustomerCart(ctx, &sh.CustomerID)
			switch {
			case err == nil:
				existing = true
			case errors.Is(err, pgx.ErrNoRows):
				id = newID()
				err = q.InsertCustomerCart(ctx, db.InsertCustomerCartParams{SiteID: req.SiteId, ID: id, CustomerID: &sh.CustomerID})
			}
		} else {
			token, id = ptr(newToken()), newID()
			err = q.InsertGuestCart(ctx, db.InsertGuestCartParams{SiteID: req.SiteId, ID: id, TokenHash: hashToken(*token)})
		}
		if err != nil {
			return err
		}
		cart, err = priceCart(ctx, q, id, site.Currency)
		return err
	})
	if err != nil {
		return nil, err
	}
	body := oapi.NewCart{Cart: cart.json(), CartToken: token}
	if existing {
		return oapi.CreateCart200JSONResponse(body), nil
	}
	return oapi.CreateCart201JSONResponse(body), nil
}

// MergeCart moves a guest cart's lines (X-Cart-Token) into the signed-in
// shopper's cart, adding quantities on identical lines.
func (s *Server) MergeCart(ctx context.Context, req oapi.MergeCartRequestObject) (oapi.MergeCartResponseObject, error) {
	sh, err := s.requireShopper(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	if req.Params.XCartToken == nil {
		return nil, errNotFound
	}
	var cart pricedCart
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		guest, err := q.GuestCartForUpdate(ctx, hashToken(*req.Params.XCartToken))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		cartID, err := q.CustomerCart(ctx, &sh.CustomerID)
		if errors.Is(err, pgx.ErrNoRows) {
			// Adopt the guest cart as the shopper's own.
			if err := q.AdoptCart(ctx, db.AdoptCartParams{ID: guest, CustomerID: &sh.CustomerID}); err != nil {
				return err
			}
			cart, err = priceCart(ctx, q, guest, site.Currency)
			return err
		}
		if err != nil {
			return err
		}
		lines, err := q.CartLines(ctx, guest)
		if err != nil {
			return err
		}
		for _, l := range lines {
			if err := q.MergeCartLine(ctx, db.MergeCartLineParams{
				SiteID: req.SiteId, ID: newID(), CartID: cartID, VariantID: l.VariantID, Quantity: l.Quantity,
				OptionValueIds: l.OptionValueIds, UnitPrice: l.UnitPrice, MaxQuantity: maxQuantity,
			}); err != nil {
				return err
			}
		}
		if err := q.SetCartStatus(ctx, db.SetCartStatusParams{ID: guest, Status: "merged"}); err != nil {
			return err
		}
		if err := q.TouchCart(ctx, cartID); err != nil {
			return err
		}
		cart, err = priceCart(ctx, q, cartID, site.Currency)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.MergeCart200JSONResponse(cart.json()), nil
}

// withCart runs fn on the caller's current cart and answers it re-priced.
func (s *Server) withCart(ctx context.Context, siteID uuid.UUID, token *string, fn func(q *db.Queries, cartID uuid.UUID) error) (oapi.Cart, error) {
	key, err := s.cartKey(ctx, siteID, token)
	if err != nil {
		return oapi.Cart{}, err
	}
	var cart pricedCart
	err = db.InSite(ctx, s.Pool, siteID, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		cartID, err := resolveCart(ctx, q, key)
		if err != nil {
			return err
		}
		if fn != nil {
			if err := fn(q, cartID); err != nil {
				return err
			}
			if err := q.TouchCart(ctx, cartID); err != nil {
				return err
			}
		}
		cart, err = priceCart(ctx, q, cartID, site.Currency)
		return err
	})
	return cart.json(), err
}

// GetCart is the current cart, re-priced from the live catalog.
func (s *Server) GetCart(ctx context.Context, req oapi.GetCartRequestObject) (oapi.GetCartResponseObject, error) {
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, nil)
	if err != nil {
		return nil, err
	}
	return oapi.GetCart200JSONResponse(cart), nil
}

// AddItem adds a variant with its chosen add-ons. Adding the same
// combination again increases that line's quantity.
func (s *Server) AddItem(ctx context.Context, req oapi.AddItemRequestObject) (oapi.AddItemResponseObject, error) {
	quantity := deref(req.Body.Quantity, 1)
	if err := checkQuantity(quantity); err != nil {
		return nil, err
	}
	options := slices.Clone(deref(req.Body.OptionValueIds, nil))
	slices.SortFunc(options, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, func(q *db.Queries, cartID uuid.UUID) error {
		unit, err := checkPurchasable(ctx, q, req.Body.VariantId, options)
		if err != nil {
			return err
		}
		total, err := q.AddCartLine(ctx, db.AddCartLineParams{
			SiteID: req.SiteId, ID: newID(), CartID: cartID, VariantID: req.Body.VariantId, Quantity: quantity,
			OptionValueIds: nonNil(options), UnitPrice: &unit,
		})
		if err != nil {
			return err
		}
		if err := checkQuantity(total); err != nil {
			return err
		}
		return checkCartStock(ctx, q, req.Body.VariantId, cartID)
	})
	if err != nil {
		return nil, err
	}
	return oapi.AddItem200JSONResponse(cart), nil
}

func (s *Server) UpdateItem(ctx context.Context, req oapi.UpdateItemRequestObject) (oapi.UpdateItemResponseObject, error) {
	if err := checkQuantity(req.Body.Quantity); err != nil {
		return nil, err
	}
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, func(q *db.Queries, cartID uuid.UUID) error {
		variantID, err := q.SetLineQuantity(ctx, db.SetLineQuantityParams{ID: req.ItemId, CartID: cartID, Quantity: req.Body.Quantity})
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		return checkCartStock(ctx, q, variantID, cartID)
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateItem200JSONResponse(cart), nil
}

func (s *Server) RemoveItem(ctx context.Context, req oapi.RemoveItemRequestObject) (oapi.RemoveItemResponseObject, error) {
	cart, err := s.withCart(ctx, req.SiteId, req.Params.XCartToken, func(q *db.Queries, cartID uuid.UUID) error {
		n, err := q.RemoveCartLine(ctx, db.RemoveCartLineParams{ID: req.ItemId, CartID: cartID})
		if err == nil && n == 0 {
			return errNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.RemoveItem200JSONResponse(cart), nil
}
