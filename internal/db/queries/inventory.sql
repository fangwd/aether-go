-- name: StockLevels :many
SELECT i.variant_id, v.product_id, p.name AS product_name, v.name AS variant_name, v.sku,
       i.on_hand, i.reserved, i.available::bigint AS available
  FROM inventory i
  JOIN product_variant v ON v.site_id = i.site_id AND v.id = i.variant_id
  JOIN product p ON p.site_id = v.site_id AND p.id = v.product_id
 WHERE (sqlc.narg(variant_id)::uuid IS NULL OR i.variant_id = sqlc.narg(variant_id))
 ORDER BY p.name, v.sort_order;

-- name: VariantExists :one
SELECT EXISTS (SELECT 1 FROM product_variant WHERE id = $1);

-- name: TrackStock :exec
INSERT INTO inventory (site_id, variant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: AdjustOnHand :execrows
UPDATE inventory SET on_hand = on_hand + @quantity::bigint, version = version + 1
 WHERE variant_id = @variant_id AND on_hand + @quantity::bigint >= reserved;

-- name: RecordStaffAdjustment :exec
INSERT INTO inventory_transaction (site_id, id, variant_id, transaction_type, quantity, reference_type, metadata)
VALUES ($1, $2, $3, $4, $5, 'staff', jsonb_build_object('subject', @subject::text, 'note', sqlc.narg(note)::text));

-- name: HoldStock :execrows
UPDATE inventory SET reserved = reserved + @quantity::bigint, version = version + 1
 WHERE variant_id = @variant_id AND on_hand - reserved >= @quantity::bigint;

-- name: IsTracked :one
SELECT EXISTS (SELECT 1 FROM inventory WHERE variant_id = $1);

-- name: InsertReservation :exec
INSERT INTO inventory_reservation (site_id, id, variant_id, order_id, quantity) VALUES ($1, $2, $3, $4, $5);

-- name: ReleaseReservations :exec
WITH released AS (
    UPDATE inventory_reservation SET status = 'released'
     WHERE order_id = $1 AND status = 'active'
    RETURNING variant_id, quantity
)
UPDATE inventory i SET reserved = i.reserved - r.quantity, version = i.version + 1
  FROM (SELECT variant_id, sum(quantity)::bigint AS quantity FROM released GROUP BY variant_id) r
 WHERE i.variant_id = r.variant_id;

-- name: ConvertReservations :many
WITH converted AS (
    UPDATE inventory_reservation SET status = 'converted'
     WHERE order_id = $1 AND status = 'active'
    RETURNING variant_id, quantity
)
SELECT variant_id, sum(quantity)::bigint AS quantity FROM converted GROUP BY variant_id;

-- name: SellHeld :exec
UPDATE inventory SET on_hand = on_hand - @quantity::bigint, reserved = reserved - @quantity::bigint, version = version + 1
 WHERE variant_id = @variant_id;

-- name: RecordSale :exec
INSERT INTO inventory_transaction (site_id, id, variant_id, transaction_type, quantity, reference_type, reference_id)
VALUES ($1, $2, $3, 'sale', $4, 'order', $5)
ON CONFLICT DO NOTHING;

-- name: Restock :execrows
UPDATE inventory SET on_hand = on_hand + @quantity::bigint, version = version + 1 WHERE variant_id = @variant_id;

-- name: RecordReturn :exec
INSERT INTO inventory_transaction (site_id, id, variant_id, transaction_type, quantity, reference_type, reference_id)
VALUES ($1, $2, $3, 'return', $4, 'refund', $5);
