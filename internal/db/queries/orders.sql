-- name: OrderByID :one
SELECT id, order_number, customer_id, customer_email::text AS email, currency, payment_status, fulfilment_status,
       shipping_method_code, subtotal, discount_total, shipping_total, shipping_tax, tax_total, grand_total,
       refunded_total, prices_include_tax, placed_at, cancelled_at
  FROM "order" WHERE id = $1;

-- name: OrderItems :many
SELECT oi.id, oi.product_id, oi.variant_id, oi.sku, oi.product_name, oi.variant_name, oi.options, oi.quantity,
       oi.unit_price, oi.discount_total, oi.tax_total, oi.total, oi.refunded_quantity,
       (SELECT COALESCE(sum(si.quantity), 0) FROM shipment_item si WHERE si.order_item_id = oi.id)::bigint AS shipped
  FROM order_item oi WHERE oi.order_id = $1 ORDER BY oi.created_at, oi.id;

-- name: OrderAddresses :many
SELECT type, first_name, last_name, company, line1, line2, city, state, postal_code, country_code, phone
  FROM order_address WHERE order_id = $1;

-- name: OrderShipmentItems :many
SELECT shipment_id, order_item_id, quantity FROM shipment_item WHERE order_id = $1;

-- name: OrderShipments :many
SELECT id, status, carrier, service, tracking_number, shipped_at, delivered_at
  FROM shipment WHERE order_id = $1 ORDER BY created_at, id;

-- name: OrderHistory :many
SELECT status_type, from_status, to_status, reason, actor_subject, created_at
  FROM order_status_history WHERE order_id = $1 ORDER BY created_at, id;

-- name: RecordStatus :exec
INSERT INTO order_status_history (site_id, id, order_id, status_type, from_status, to_status, reason, actor_subject)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: InsertOrderAddress :exec
INSERT INTO order_address (site_id, id, order_id, type, first_name, last_name, company, line1, line2, city,
                           state, postal_code, country_code, phone)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14);

-- name: Emit :exec
INSERT INTO outbox_event (site_id, id, topic, event_key, payload) VALUES ($1, $2, $3, $4, $5);

-- name: OrderStatusesForUpdate :one
SELECT payment_status, fulfilment_status FROM "order" WHERE id = $1 FOR UPDATE;

-- name: MarkCancelled :exec
UPDATE "order" SET fulfilment_status = 'cancelled', cancelled_at = now() WHERE id = $1;

-- name: OrderSummaries :many
SELECT o.id, o.order_number, o.customer_email::text AS email, o.currency, o.grand_total,
       o.payment_status, o.fulfilment_status, o.placed_at,
       (SELECT COALESCE(sum(quantity), 0) FROM order_item oi WHERE oi.order_id = o.id)::bigint AS item_count
  FROM "order" o
 WHERE (sqlc.narg(payment_status)::text IS NULL OR o.payment_status = sqlc.narg(payment_status))
   AND (sqlc.narg(fulfilment_status)::text IS NULL OR o.fulfilment_status = sqlc.narg(fulfilment_status))
   AND (sqlc.narg(cursor)::uuid IS NULL OR o.id < sqlc.narg(cursor))
   AND (sqlc.narg(customer_id)::uuid IS NULL OR o.customer_id = sqlc.narg(customer_id))
 ORDER BY o.id DESC LIMIT @lim;

-- name: FulfilmentStatusForUpdate :one
SELECT fulfilment_status FROM "order" WHERE id = $1 FOR UPDATE;

-- name: RemainingToShip :many
SELECT oi.id, (oi.quantity - (SELECT COALESCE(sum(si.quantity), 0) FROM shipment_item si WHERE si.order_item_id = oi.id))::bigint AS remaining
  FROM order_item oi WHERE oi.order_id = $1;

-- name: InsertShipment :exec
INSERT INTO shipment (site_id, id, order_id, carrier, service, tracking_number, status, shipped_at)
VALUES ($1, $2, $3, $4, $5, $6, 'in_transit', now());

-- name: InsertShipmentItem :exec
INSERT INTO shipment_item (site_id, shipment_id, order_item_id, order_id, quantity) VALUES ($1, $2, $3, $4, $5);

-- name: SetFulfilmentStatus :exec
UPDATE "order" SET fulfilment_status = $2 WHERE id = $1;

-- name: UpdateShipment :execrows
UPDATE shipment SET status = @status::text, tracking_number = COALESCE(sqlc.narg(tracking_number), tracking_number),
       delivered_at = CASE WHEN @status::text = 'delivered' THEN now() ELSE delivered_at END
 WHERE id = @id AND order_id = @order_id;

-- name: OrderTokenMatches :one
SELECT EXISTS (SELECT 1 FROM "order" WHERE id = $1 AND access_token_hash = $2);

-- name: OrderOfCustomer :one
SELECT EXISTS (SELECT 1 FROM "order" WHERE id = $1 AND customer_id = $2);

-- name: NoteCatalogChanged :exec
-- At most one waiting notice per site (RLS scopes the check); a parked one
-- (failed for good) does not block new ones.
INSERT INTO outbox_event (site_id, id, topic, payload, next_retry_at)
SELECT @site_id, @id, 'catalog.changed', '{}'::jsonb, now() + make_interval(secs => @delay_secs::int)
 WHERE NOT EXISTS (SELECT 1 FROM outbox_event
                    WHERE topic = 'catalog.changed' AND published_at IS NULL AND next_retry_at <> 'infinity');
