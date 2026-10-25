-- name: GuestCart :one
SELECT id FROM cart
 WHERE token_hash = $1 AND status = 'active' AND (expires_at IS NULL OR expires_at > now());

-- name: GuestCartStatus :one
SELECT status FROM cart WHERE token_hash = $1;

-- name: CustomerCart :one
SELECT id FROM cart
 WHERE customer_id = $1 AND status = 'active' AND (expires_at IS NULL OR expires_at > now());

-- name: TouchCart :exec
UPDATE cart SET expires_at = now() + interval '30 days' WHERE id = $1;

-- name: InsertCustomerCart :exec
INSERT INTO cart (site_id, id, customer_id, expires_at) VALUES ($1, $2, $3, now() + interval '30 days');

-- name: InsertGuestCart :exec
INSERT INTO cart (site_id, id, token_hash, expires_at) VALUES ($1, $2, $3, now() + interval '30 days');

-- name: GuestCartForUpdate :one
SELECT id FROM cart WHERE token_hash = $1 AND status = 'active' FOR UPDATE;

-- name: AdoptCart :exec
UPDATE cart SET customer_id = $2, token_hash = NULL WHERE id = $1;

-- name: CartLines :many
SELECT variant_id, quantity, option_value_ids, unit_price FROM cart_item WHERE cart_id = $1;

-- name: MergeCartLine :exec
INSERT INTO cart_item (site_id, id, cart_id, variant_id, quantity, option_value_ids, unit_price)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (site_id, cart_id, variant_id, option_value_ids)
DO UPDATE SET quantity = LEAST(cart_item.quantity + EXCLUDED.quantity, @max_quantity::int);

-- name: SetCartStatus :exec
UPDATE cart SET status = $2 WHERE id = $1;

-- name: AddCartLine :one
INSERT INTO cart_item (site_id, id, cart_id, variant_id, quantity, option_value_ids, unit_price)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (site_id, cart_id, variant_id, option_value_ids)
DO UPDATE SET quantity = cart_item.quantity + EXCLUDED.quantity, unit_price = EXCLUDED.unit_price
RETURNING quantity;

-- name: SetLineQuantity :one
UPDATE cart_item SET quantity = $3 WHERE id = $1 AND cart_id = $2 RETURNING variant_id;

-- name: RemoveCartLine :execrows
DELETE FROM cart_item WHERE id = $1 AND cart_id = $2;

-- name: PurchasableVariant :one
SELECT v.product_id, v.price_extra, p.price
  FROM product_variant v
  JOIN product p ON p.site_id = v.site_id AND p.id = v.product_id
 WHERE v.id = $1 AND v.status = 'active' AND p.status = 'active';

-- name: ProductOptions :many
SELECT g.product_id, g.id AS group_id, g.name AS group_name, g.min_select, g.max_select,
       v.id AS value_id, v.label, v.price_extra
  FROM product_option_group g
  JOIN product_option_value v ON v.site_id = g.site_id AND v.group_id = g.id
 WHERE g.product_id = ANY(@product_ids::uuid[])
 ORDER BY g.sort_order, g.id, v.sort_order, v.id;

-- name: CartStock :one
SELECT i.available::bigint AS available,
       (SELECT COALESCE(sum(quantity), 0) FROM cart_item c WHERE c.cart_id = @cart_id AND c.variant_id = @variant_id)::bigint AS wanted
  FROM inventory i WHERE i.variant_id = @variant_id;

-- name: PriceCartLines :many
SELECT ci.id, ci.variant_id, ci.quantity, ci.option_value_ids,
       v.product_id, v.name AS variant_name, v.attributes, v.price_extra, v.status AS variant_status,
       p.slug, p.name AS product_name, p.price, p.status AS product_status, p.tax_class_id,
       i.available,
       (SELECT sum(c2.quantity) FROM cart_item c2 WHERE c2.cart_id = ci.cart_id AND c2.variant_id = ci.variant_id)::bigint AS variant_quantity
  FROM cart_item ci
  JOIN product_variant v ON v.site_id = ci.site_id AND v.id = ci.variant_id
  JOIN product p ON p.site_id = v.site_id AND p.id = v.product_id
  LEFT JOIN inventory i ON i.site_id = v.site_id AND i.variant_id = v.id
 WHERE ci.cart_id = $1
 ORDER BY ci.id; -- UUIDv7: the order lines were added

-- name: CartCoupon :one
SELECT coupon_code FROM cart WHERE id = $1;
