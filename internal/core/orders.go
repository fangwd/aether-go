package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
	"aether/internal/ports"
)

type orderView int

const (
	adminOrderView orderView = iota
	customerOrderView
)

// loadOrder is an order with its items, addresses and shipments; admins
// also see the customer and the status history.
func loadOrder(ctx context.Context, q *db.Queries, orderID uuid.UUID, view orderView) (oapi.Order, error) {
	o, err := q.OrderByID(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return oapi.Order{}, errNotFound
	}
	if err != nil {
		return oapi.Order{}, err
	}
	c := o.Currency
	out := oapi.Order{
		Id: o.ID, OrderNumber: o.OrderNumber, Email: o.Email, Currency: c, PaymentStatus: o.PaymentStatus,
		FulfilmentStatus: o.FulfilmentStatus, ShippingMethodCode: o.ShippingMethodCode,
		Subtotal: money(o.Subtotal, c), DiscountTotal: money(o.DiscountTotal, c), ShippingTotal: money(o.ShippingTotal, c),
		ShippingTax: money(o.ShippingTax, c), TaxTotal: money(o.TaxTotal, c), GrandTotal: money(o.GrandTotal, c),
		RefundedTotal: money(o.RefundedTotal, c), PricesIncludeTax: o.PricesIncludeTax,
		PlacedAt: o.PlacedAt, CancelledAt: o.CancelledAt, Items: []oapi.OrderItem{}, Shipments: []oapi.Shipment{},
	}
	items, err := q.OrderItems(ctx, orderID)
	if err != nil {
		return out, err
	}
	for _, i := range items {
		var options []map[string]any
		_ = json.Unmarshal(i.Options, &options)
		out.Items = append(out.Items, oapi.OrderItem{
			Id: i.ID, ProductId: i.ProductID, VariantId: i.VariantID, Sku: i.Sku, ProductName: i.ProductName,
			VariantName: i.VariantName, Options: nonNil(options), Quantity: i.Quantity, UnitPrice: money(i.UnitPrice, c),
			DiscountTotal: money(i.DiscountTotal, c), TaxTotal: money(i.TaxTotal, c), Total: money(i.Total, c),
			ShippedQuantity: i.Shipped, RefundedQuantity: i.RefundedQuantity,
		})
	}
	addresses, err := q.OrderAddresses(ctx, orderID)
	if err != nil {
		return out, err
	}
	for _, a := range addresses {
		addr := &oapi.Address{FirstName: a.FirstName, LastName: a.LastName, Company: a.Company, Line1: a.Line1, Line2: a.Line2,
			City: a.City, State: a.State, PostalCode: a.PostalCode, CountryCode: a.CountryCode, Phone: a.Phone}
		if a.Type == "shipping" {
			out.ShippingAddress = addr
		} else {
			out.BillingAddress = addr
		}
	}
	shipmentItems := map[uuid.UUID][]oapi.ShipmentItem{}
	sis, err := q.OrderShipmentItems(ctx, orderID)
	if err != nil {
		return out, err
	}
	for _, si := range sis {
		shipmentItems[si.ShipmentID] = append(shipmentItems[si.ShipmentID], oapi.ShipmentItem{OrderItemId: si.OrderItemID, Quantity: si.Quantity})
	}
	shipments, err := q.OrderShipments(ctx, orderID)
	if err != nil {
		return out, err
	}
	for _, s := range shipments {
		out.Shipments = append(out.Shipments, oapi.Shipment{Id: s.ID, Status: s.Status, Carrier: s.Carrier, Service: s.Service,
			TrackingNumber: s.TrackingNumber, Items: nonNil(shipmentItems[s.ID]), ShippedAt: s.ShippedAt, DeliveredAt: s.DeliveredAt})
	}
	if view == adminOrderView {
		out.CustomerId = o.CustomerID
		history, err := q.OrderHistory(ctx, orderID)
		if err != nil {
			return out, err
		}
		changes := []oapi.StatusChange{}
		for _, h := range history {
			changes = append(changes, oapi.StatusChange{StatusType: h.StatusType, FromStatus: h.FromStatus, ToStatus: h.ToStatus,
				Reason: h.Reason, Actor: h.ActorSubject, At: h.CreatedAt})
		}
		out.History = &changes
	}
	return out, nil
}

// transition is one entry in an order's status trail; actor is who made
// the change, nil for the system.
type transition struct {
	kind   string // payment or fulfilment
	from   *string
	to     string
	reason *string
	actor  *string
}

func recordStatus(ctx context.Context, q *db.Queries, siteID, orderID uuid.UUID, t transition) error {
	return q.RecordStatus(ctx, db.RecordStatusParams{SiteID: siteID, ID: newID(), OrderID: orderID, StatusType: t.kind,
		FromStatus: t.from, ToStatus: t.to, Reason: t.reason, ActorSubject: t.actor})
}

// emit queues an event for the worker, in the caller's transaction.
func emit(ctx context.Context, q *db.Queries, siteID uuid.UUID, topic string, key uuid.UUID, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	k := key.String()
	return q.Emit(ctx, db.EmitParams{SiteID: siteID, ID: newID(), Topic: topic, EventKey: &k, Payload: b})
}

func insertAddress(ctx context.Context, q *db.Queries, siteID, orderID uuid.UUID, kind string, a *oapi.Address) error {
	return q.InsertOrderAddress(ctx, db.InsertOrderAddressParams{SiteID: siteID, ID: newID(), OrderID: orderID, Type: kind,
		FirstName: a.FirstName, LastName: a.LastName, Company: a.Company, Line1: a.Line1, Line2: a.Line2, City: a.City,
		State: a.State, PostalCode: a.PostalCode, CountryCode: a.CountryCode, Phone: a.Phone})
}

// cancelOrder cancels an unfulfilled order that is unpaid (or fully
// refunded): it stops any open payment, releases held stock and promotion
// uses, and records who cancelled it (nil actor for the system).
func cancelOrder(ctx context.Context, q *db.Queries, payments ports.PaymentGateway, siteID, orderID uuid.UUID, reason, actor *string) error {
	o, err := q.OrderStatusesForUpdate(ctx, orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	if o.FulfilmentStatus != "unfulfilled" {
		return conflict("a %s order cannot be cancelled", o.FulfilmentStatus)
	}
	if o.PaymentStatus != "pending" && o.PaymentStatus != "refunded" {
		return conflict("refund the payment before cancelling")
	}
	if err := cancelOpenPayments(ctx, q, payments, orderID); err != nil {
		return err
	}
	if err := releaseStock(ctx, q, orderID); err != nil {
		return err
	}
	if err := releasePromotions(ctx, q, orderID); err != nil {
		return err
	}
	if err := q.MarkCancelled(ctx, orderID); err != nil {
		return err
	}
	if err := recordStatus(ctx, q, siteID, orderID, transition{kind: "fulfilment", from: ptr("unfulfilled"), to: "cancelled", reason: reason, actor: actor}); err != nil {
		return err
	}
	return emit(ctx, q, siteID, "order.cancelled", orderID, map[string]any{"order_id": orderID})
}

func summariesJSON(rows []db.OrderSummariesRow) []oapi.OrderSummary {
	out := []oapi.OrderSummary{}
	for _, r := range rows {
		out = append(out, oapi.OrderSummary{Id: r.ID, OrderNumber: r.OrderNumber, Email: r.Email, Currency: r.Currency,
			GrandTotal: money(r.GrandTotal, r.Currency), PaymentStatus: r.PaymentStatus, FulfilmentStatus: r.FulfilmentStatus,
			ItemCount: r.ItemCount, PlacedAt: r.PlacedAt})
	}
	return out
}

// ListOrders is the site's orders, newest first.
func (s *Server) ListOrders(ctx context.Context, req oapi.ListOrdersRequestObject) (oapi.ListOrdersResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	limit := pageSize(req.Params.Limit)
	var page oapi.OrderSummaryPage
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		rows, err := q.OrderSummaries(ctx, db.OrderSummariesParams{PaymentStatus: req.Params.PaymentStatus,
			FulfilmentStatus: req.Params.FulfilmentStatus, Cursor: req.Params.Cursor, Lim: int32(limit + 1)})
		if err != nil {
			return err
		}
		if int64(len(rows)) > limit {
			rows = rows[:limit]
			page.NextCursor = &rows[len(rows)-1].ID
		}
		page.Items = summariesJSON(rows)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return oapi.ListOrders200JSONResponse(page), nil
}

func (s *Server) AdminGetOrder(ctx context.Context, req oapi.AdminGetOrderRequestObject) (oapi.AdminGetOrderResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var order oapi.Order
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		order, err = loadOrder(ctx, q, req.OrderId, adminOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.AdminGetOrder200JSONResponse(order), nil
}

// CancelOrder cancels an unfulfilled, unpaid order and releases its held
// stock.
func (s *Server) CancelOrder(ctx context.Context, req oapi.CancelOrderRequestObject) (oapi.CancelOrderResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	var order oapi.Order
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if err := cancelOrder(ctx, q, s.Payments, req.SiteId, req.OrderId, req.Body.Reason, &staff.Subject); err != nil {
			return err
		}
		var err error
		order, err = loadOrder(ctx, q, req.OrderId, adminOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CancelOrder200JSONResponse(order), nil
}

// CreateShipment records a parcel (or a pickup handover) for some or all of
// an order; omitted items mean everything not yet shipped.
func (s *Server) CreateShipment(ctx context.Context, req oapi.CreateShipmentRequestObject) (oapi.CreateShipmentResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	orderID := req.OrderId
	var order oapi.Order
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		status, err := q.FulfilmentStatusForUpdate(ctx, orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound
		}
		if err != nil {
			return err
		}
		if status == "cancelled" || status == "fulfilled" {
			return conflict("the order is %s", status)
		}
		remaining, err := q.RemainingToShip(ctx, orderID)
		if err != nil {
			return err
		}
		items := deref(req.Body.Items, nil)
		if len(items) == 0 {
			for _, r := range remaining {
				if r.Remaining > 0 {
					items = append(items, oapi.ShipmentItem{OrderItemId: r.ID, Quantity: int32(r.Remaining)})
				}
			}
		}
		if len(items) == 0 {
			return unprocessable("a shipment needs items with positive quantities")
		}
		left := map[uuid.UUID]int64{}
		for _, r := range remaining {
			left[r.ID] = r.Remaining
		}
		shipping := map[uuid.UUID]int64{}
		for _, i := range items {
			if i.Quantity < 1 {
				return unprocessable("a shipment needs items with positive quantities")
			}
			l, ok := left[i.OrderItemId]
			if !ok {
				return unprocessable("an item is not part of this order")
			}
			if int64(i.Quantity) > l {
				return unprocessable("that would ship more than was ordered")
			}
			shipping[i.OrderItemId] += int64(i.Quantity)
		}
		shipmentID := newID()
		if err := q.InsertShipment(ctx, db.InsertShipmentParams{SiteID: req.SiteId, ID: shipmentID, OrderID: orderID,
			Carrier: req.Body.Carrier, Service: req.Body.Service, TrackingNumber: req.Body.TrackingNumber}); err != nil {
			return err
		}
		for _, i := range items {
			if err := q.InsertShipmentItem(ctx, db.InsertShipmentItemParams{SiteID: req.SiteId, ShipmentID: shipmentID,
				OrderItemID: i.OrderItemId, OrderID: orderID, Quantity: i.Quantity}); err != nil {
				return err
			}
		}
		next := "fulfilled"
		for _, r := range remaining {
			if r.Remaining != shipping[r.ID] {
				next = "partially_fulfilled"
			}
		}
		if next != status {
			if err := q.SetFulfilmentStatus(ctx, db.SetFulfilmentStatusParams{ID: orderID, FulfilmentStatus: next}); err != nil {
				return err
			}
			if err := recordStatus(ctx, q, req.SiteId, orderID, transition{kind: "fulfilment", from: &status, to: next, actor: &staff.Subject}); err != nil {
				return err
			}
		}
		if err := emit(ctx, q, req.SiteId, "order.shipped", orderID, map[string]any{"order_id": orderID, "shipment_id": shipmentID}); err != nil {
			return err
		}
		order, err = loadOrder(ctx, q, orderID, adminOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.CreateShipment201JSONResponse(order), nil
}

func (s *Server) UpdateShipment(ctx context.Context, req oapi.UpdateShipmentRequestObject) (oapi.UpdateShipmentResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var order oapi.Order
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		n, err := q.UpdateShipment(ctx, db.UpdateShipmentParams{Status: string(req.Body.Status), TrackingNumber: req.Body.TrackingNumber,
			ID: req.ShipmentId, OrderID: req.OrderId})
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		order, err = loadOrder(ctx, q, req.OrderId, adminOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.UpdateShipment200JSONResponse(order), nil
}

// MyOrders is the signed-in shopper's orders, newest first (including guest
// orders placed with their email once it is verified).
func (s *Server) MyOrders(ctx context.Context, req oapi.MyOrdersRequestObject) (oapi.MyOrdersResponseObject, error) {
	sh, err := s.requireShopper(ctx, req.SiteId)
	if err != nil {
		return nil, err
	}
	var rows []db.OrderSummariesRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		rows, err = q.OrderSummaries(ctx, db.OrderSummariesParams{CustomerID: &sh.CustomerID, Lim: 1 << 30})
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.MyOrders200JSONResponse(summariesJSON(rows)), nil
}

// orderAccess is proof the caller may see an order: a signed-in shopper or
// a guest's order token.
type orderAccess struct {
	shopper   *Shopper
	tokenHash []byte
}

func (s *Server) orderAccess(ctx context.Context, siteID uuid.UUID, token *string) (orderAccess, error) {
	sh, err := s.shopper(ctx, siteID)
	if err != nil || sh != nil {
		return orderAccess{shopper: sh}, err
	}
	if token == nil || *token == "" {
		return orderAccess{}, errNotFound
	}
	return orderAccess{tokenHash: hashToken(*token)}, nil
}

// checkOrderAccess is 404 unless the caller may see this order.
func checkOrderAccess(ctx context.Context, q *db.Queries, a orderAccess, orderID uuid.UUID) error {
	var ok bool
	var err error
	if a.shopper != nil {
		ok, err = q.OrderOfCustomer(ctx, db.OrderOfCustomerParams{ID: orderID, CustomerID: &a.shopper.CustomerID})
	} else {
		ok, err = q.OrderTokenMatches(ctx, db.OrderTokenMatchesParams{ID: orderID, AccessTokenHash: a.tokenHash})
	}
	if err == nil && !ok {
		return errNotFound
	}
	return err
}

// GetOrder is one order: the shopper's own, or a guest's with the order
// token from checkout.
func (s *Server) GetOrder(ctx context.Context, req oapi.GetOrderRequestObject) (oapi.GetOrderResponseObject, error) {
	access, err := s.orderAccess(ctx, req.SiteId, req.Params.XOrderToken)
	if err != nil {
		return nil, err
	}
	var order oapi.Order
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if err := checkOrderAccess(ctx, q, access, req.OrderId); err != nil {
			return err
		}
		var err error
		order, err = loadOrder(ctx, q, req.OrderId, customerOrderView)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.GetOrder200JSONResponse(order), nil
}
