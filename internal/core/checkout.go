package core

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

// Checkout places an order for the current cart. Send a fresh
// Idempotency-Key per attempt; retrying with the same key returns the same
// order.
func (s *Server) Checkout(ctx context.Context, req oapi.CheckoutRequestObject) (oapi.CheckoutResponseObject, error) {
	key, err := s.cartKey(ctx, req.SiteId, req.Params.XCartToken)
	if err != nil {
		return nil, err
	}
	body := req.Body
	var email string
	switch {
	case body.Email != nil:
		if email, err = normalizeEmail(*body.Email); err != nil {
			return nil, err
		}
	case key.shopper != nil:
		email = key.shopper.Email
	default:
		return nil, unprocessable("email is required")
	}
	for _, a := range []*oapi.Address{body.ShippingAddress, body.BillingAddress} {
		if err := checkAddress(a); err != nil {
			return nil, err
		}
	}
	siteID := req.SiteId
	var placed oapi.Placed
	var orderID uuid.UUID
	var orderNumber int64
	err = db.InSite(ctx, s.Pool, siteID, func(_ pgx.Tx, q *db.Queries) error {
		site, err := loadSite(ctx, q)
		if err != nil {
			return err
		}
		replay, err := idempotencyClaim(ctx, q, siteID, req.Params.IdempotencyKey, key.fingerprint())
		if err != nil {
			return err
		}
		if replay != nil {
			return json.Unmarshal(replay, &placed)
		}
		cartID, err := resolveCartForCheckout(ctx, q, key)
		if err != nil {
			return err
		}
		if err := q.LockCart(ctx, cartID); err != nil {
			return err
		}
		cart, err := priceCart(ctx, q, cartID, site.Currency)
		if err != nil {
			return err
		}
		if !cart.Ready {
			var problems []string
			for _, l := range cart.Items {
				if l.Problem != nil {
					problems = append(problems, l.ProductName+": "+*l.Problem)
				}
			}
			if len(problems) == 0 {
				return unprocessable("the cart is empty")
			}
			return unprocessable("%s", strings.Join(problems, "; "))
		}

		var customerID uuid.UUID
		var orderToken *string
		if key.shopper != nil {
			customerID = key.shopper.CustomerID
		} else {
			if customerID, err = q.GuestCustomer(ctx, db.GuestCustomerParams{SiteID: siteID, ID: newID(), Email: &email}); err != nil {
				return err
			}
			orderToken = ptr(newToken())
		}
		quoted, err := priceOrder(ctx, q, site, cart, destination{shipping: body.ShippingAddress, billing: body.BillingAddress, methodCode: body.ShippingMethodCode}, &customerID)
		if err != nil {
			return err
		}
		if cart.CouponCode != nil && !quoted.couponApplied {
			return unprocessable("the coupon can no longer be used here; remove it to continue")
		}
		ships, err := q.HasShippingMethods(ctx)
		if err != nil {
			return err
		}
		if ships {
			if quoted.method == nil {
				return unprocessable("choose a shipping method from the quote")
			}
			if quoted.method.Kind == "delivery" && body.ShippingAddress == nil {
				return unprocessable("a shipping address is required for delivery")
			}
		}

		orderID = newID()
		wanted := map[uuid.UUID]int64{}
		var variantIDs []uuid.UUID
		for _, l := range cart.Items {
			if _, seen := wanted[l.VariantID]; !seen {
				variantIDs = append(variantIDs, l.VariantID)
			}
			wanted[l.VariantID] += int64(l.Quantity)
		}
		if err := reserveStock(ctx, q, siteID, orderID, wanted); err != nil {
			return err
		}
		if orderNumber, err = q.NextOrderNumber(ctx); err != nil {
			return err
		}
		var tokenHash []byte
		if orderToken != nil {
			tokenHash = hashToken(*orderToken)
		}
		var methodCode *string
		if quoted.method != nil {
			methodCode = &quoted.method.Code
		}
		t := quoted.totals
		if err := q.InsertOrder(ctx, db.InsertOrderParams{
			SiteID: siteID, ID: orderID, CustomerID: &customerID, CustomerEmail: email, OrderNumber: orderNumber,
			AccessTokenHash: tokenHash, Currency: site.Currency, ShippingMethodCode: methodCode, Subtotal: t.Subtotal,
			DiscountTotal: t.DiscountTotal, ShippingTotal: t.ShippingTotal, ShippingTax: t.ShippingTax, TaxTotal: t.TaxTotal,
			GrandTotal: t.GrandTotal, PricesIncludeTax: site.PricesIncludeTax, DiscountSnapshot: quoted.discountSnapshot(),
		}); err != nil {
			return err
		}
		if err := redeemPromotions(ctx, q, siteID, orderID, customerID, quoted.redemptions); err != nil {
			return err
		}
		skuRows, err := q.VariantSkus(ctx, variantIDs)
		if err != nil {
			return err
		}
		skus := map[uuid.UUID]*string{}
		for _, r := range skuRows {
			skus[r.ID] = r.Sku
		}
		// A ready cart has no problem lines, so lines, line tax and line
		// discounts align.
		for i, l := range cart.Items {
			tax, discount := t.LineTax[i], quoted.lineDiscounts[i]
			total := l.LineTotal.Sub(discount)
			if !site.PricesIncludeTax {
				total = total.Add(tax)
			}
			options := []map[string]any{}
			for _, o := range l.Options {
				options = append(options, map[string]any{"group": o.Group, "value_id": o.ValueID, "label": o.Label, "price_extra": o.PriceExtra.String()})
			}
			optionsJSON, _ := json.Marshal(options)
			snapshot, _ := json.Marshal(map[string]any{"slug": l.ProductSlug, "attributes": l.Attributes})
			if err := q.InsertOrderItem(ctx, db.InsertOrderItemParams{
				SiteID: siteID, ID: newID(), OrderID: orderID, ProductID: &l.ProductID, VariantID: &l.VariantID,
				Sku: skus[l.VariantID], ProductName: l.ProductName, VariantName: l.VariantName, Quantity: l.Quantity,
				UnitPrice: l.UnitPrice, DiscountTotal: discount, TaxTotal: tax, Total: total,
				ProductSnapshot: snapshot, Options: optionsJSON,
			}); err != nil {
				return err
			}
		}
		if body.ShippingAddress != nil {
			if err := insertAddress(ctx, q, siteID, orderID, "shipping", body.ShippingAddress); err != nil {
				return err
			}
		}
		if billing := firstAddress(body.BillingAddress, body.ShippingAddress); billing != nil {
			if err := insertAddress(ctx, q, siteID, orderID, "billing", billing); err != nil {
				return err
			}
		}
		for _, st := range []struct{ kind, to string }{{"payment", "pending"}, {"fulfilment", "unfulfilled"}} {
			if err := recordStatus(ctx, q, siteID, orderID, transition{kind: st.kind, to: st.to, reason: ptr("order placed")}); err != nil {
				return err
			}
		}
		if err := q.SetCartStatus(ctx, db.SetCartStatusParams{ID: cartID, Status: "converted"}); err != nil {
			return err
		}
		if err := emit(ctx, q, siteID, "order.placed", orderID, map[string]any{"order_id": orderID}); err != nil {
			return err
		}
		order, err := loadOrder(ctx, q, orderID, customerOrderView)
		if err != nil {
			return err
		}
		placed = oapi.Placed{Order: order, OrderToken: orderToken}
		return idempotencyComplete(ctx, q, req.Params.IdempotencyKey, http.StatusCreated, placed)
	})
	if err != nil {
		return nil, err
	}
	if orderID != uuid.Nil {
		slog.Info("order placed", "order_id", orderID, "order_number", orderNumber)
	}
	return oapi.Checkout201JSONResponse(placed), nil
}

func firstAddress(as ...*oapi.Address) *oapi.Address {
	for _, a := range as {
		if a != nil {
			return a
		}
	}
	return nil
}
