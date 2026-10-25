-- name: ClaimIdempotencyKey :one
INSERT INTO idempotency_key (site_id, key, request_path, expires_at)
VALUES ($1, $2, $3, now() + interval '24 hours')
ON CONFLICT DO NOTHING RETURNING key;

-- name: StoredIdempotencyKey :one
SELECT request_path, response_status, response_body FROM idempotency_key WHERE key = $1;

-- name: CompleteIdempotencyKey :exec
UPDATE idempotency_key SET response_status = $2, response_body = $3 WHERE key = $1;

-- name: LockCart :exec
SELECT id FROM cart WHERE id = $1 FOR UPDATE;

-- name: GuestCustomer :one
INSERT INTO customer (site_id, id, email) VALUES ($1, $2, $3)
ON CONFLICT (site_id, email) WHERE email IS NOT NULL AND password_hash IS NULL DO UPDATE SET email = EXCLUDED.email
RETURNING id;

-- name: NextOrderNumber :one
UPDATE site SET next_order_number = next_order_number + 1 RETURNING (next_order_number - 1)::bigint AS n;

-- name: InsertOrder :exec
INSERT INTO "order" (site_id, id, customer_id, customer_email, order_number, access_token_hash, currency,
                     shipping_method_code, subtotal, discount_total, shipping_total, shipping_tax, tax_total,
                     grand_total, prices_include_tax, discount_snapshot, placed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now());

-- name: VariantSkus :many
SELECT id, sku FROM product_variant WHERE id = ANY(@ids::uuid[]);

-- name: InsertOrderItem :exec
INSERT INTO order_item (site_id, id, order_id, product_id, variant_id, sku, product_name, variant_name,
                        quantity, unit_price, discount_total, tax_total, total, product_snapshot, options)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);
