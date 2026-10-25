package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/core/oapi"
	"aether/internal/db"
)

func stockJSON(rows []db.StockLevelsRow) []oapi.StockLevel {
	out := make([]oapi.StockLevel, len(rows))
	for i, r := range rows {
		out[i] = oapi.StockLevel{VariantId: r.VariantID, ProductId: r.ProductID, ProductName: r.ProductName,
			VariantName: stockVariantName(r), Sku: r.Sku, OnHand: r.OnHand, Reserved: r.Reserved, Available: r.Available}
	}
	return out
}

// stockVariantName is the variant's own name or, for an unnamed variant, its
// axis values in axis order ("Large / Red"); nil for a product's only variant.
func stockVariantName(r db.StockLevelsRow) *string {
	if r.VariantName != nil {
		return r.VariantName
	}
	var attributes map[string]any
	if err := json.Unmarshal(r.VariantAttributes, &attributes); err != nil {
		return nil
	}
	var values []string
	for _, axis := range r.VariantAxes {
		if v, ok := attributes[axis].(string); ok && v != "" {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return nil
	}
	name := strings.Join(values, " / ")
	return &name
}

// ListInventory is every stock-tracked variant.
func (s *Server) ListInventory(ctx context.Context, req oapi.ListInventoryRequestObject) (oapi.ListInventoryResponseObject, error) {
	if _, err := s.staff(ctx); err != nil {
		return nil, err
	}
	var rows []db.StockLevelsRow
	err := db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) (err error) {
		rows, err = q.StockLevels(ctx, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.ListInventory200JSONResponse(stockJSON(rows)), nil
}

// Adjust changes a variant's on-hand stock, through the append-only ledger.
// The first adjustment starts tracking stock for the variant; untracked
// variants are always purchasable.
func (s *Server) Adjust(ctx context.Context, req oapi.AdjustRequestObject) (oapi.AdjustResponseObject, error) {
	staff, err := s.staff(ctx)
	if err != nil {
		return nil, err
	}
	if req.Body.Quantity == 0 {
		return nil, unprocessable("quantity must not be zero")
	}
	var rows []db.StockLevelsRow
	err = db.InSite(ctx, s.Pool, req.SiteId, func(_ pgx.Tx, q *db.Queries) error {
		if exists, err := q.VariantExists(ctx, req.VariantId); err != nil || !exists {
			if err != nil {
				return err
			}
			return errNotFound
		}
		if err := q.TrackStock(ctx, db.TrackStockParams{SiteID: req.SiteId, VariantID: req.VariantId}); err != nil {
			return err
		}
		n, err := q.AdjustOnHand(ctx, db.AdjustOnHandParams{VariantID: req.VariantId, Quantity: req.Body.Quantity})
		if err != nil {
			return err
		}
		if n == 0 {
			return unprocessable("that would leave less stock than is reserved")
		}
		if err := q.RecordStaffAdjustment(ctx, db.RecordStaffAdjustmentParams{
			SiteID: req.SiteId, ID: newID(), VariantID: req.VariantId, TransactionType: string(req.Body.Kind),
			Quantity: req.Body.Quantity, Subject: staff.Subject, Note: req.Body.Note,
		}); err != nil {
			return err
		}
		if err := catalogChanged(ctx, q, req.SiteId); err != nil {
			return err
		}
		rows, err = q.StockLevels(ctx, &req.VariantId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return oapi.Adjust200JSONResponse(stockJSON(rows)[0]), nil
}

// reserveStock holds stock for an order, all or nothing. Untracked
// variants need no hold. Rows are locked in variant order so concurrent
// checkouts cannot deadlock. The hold ends when the order is paid or
// cancelled.
func reserveStock(ctx context.Context, q *db.Queries, siteID, orderID uuid.UUID, wanted map[uuid.UUID]int64) error {
	ids := make([]uuid.UUID, 0, len(wanted))
	for id := range wanted {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	for _, variantID := range ids {
		quantity := wanted[variantID]
		held, err := q.HoldStock(ctx, db.HoldStockParams{VariantID: variantID, Quantity: quantity})
		if err != nil {
			return err
		}
		if held == 0 {
			tracked, err := q.IsTracked(ctx, variantID)
			if err != nil {
				return err
			}
			if tracked {
				return conflict("an item sold out while checking out; review the cart")
			}
			continue
		}
		if err := q.InsertReservation(ctx, db.InsertReservationParams{
			SiteID: siteID, ID: newID(), VariantID: variantID, OrderID: orderID, Quantity: quantity,
		}); err != nil {
			return err
		}
	}
	return nil
}

// releaseStock gives an order's held stock back (cancelled or never paid).
func releaseStock(ctx context.Context, q *db.Queries, orderID uuid.UUID) error {
	return q.ReleaseReservations(ctx, orderID)
}

// convertStock turns an order's held stock into a sale when it is paid:
// on-hand and reserved both drop, and the ledger records the sale.
func convertStock(ctx context.Context, q *db.Queries, siteID, orderID uuid.UUID) error {
	held, err := q.ConvertReservations(ctx, orderID)
	if err != nil {
		return err
	}
	for _, h := range held {
		if err := q.SellHeld(ctx, db.SellHeldParams{VariantID: h.VariantID, Quantity: h.Quantity}); err != nil {
			return err
		}
		if err := q.RecordSale(ctx, db.RecordSaleParams{
			SiteID: siteID, ID: newID(), VariantID: h.VariantID, Quantity: -h.Quantity, ReferenceID: &orderID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// restock puts refunded units back on the shelf (tracked variants only).
func restock(ctx context.Context, q *db.Queries, siteID, variantID uuid.UUID, quantity int64, refundID uuid.UUID) error {
	n, err := q.Restock(ctx, db.RestockParams{VariantID: variantID, Quantity: quantity})
	if err != nil || n == 0 {
		return err
	}
	return q.RecordReturn(ctx, db.RecordReturnParams{SiteID: siteID, ID: newID(), VariantID: variantID, Quantity: quantity, ReferenceID: &refundID})
}
