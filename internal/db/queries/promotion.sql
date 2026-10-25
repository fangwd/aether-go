-- name: PromotionByID :one
SELECT id, name, actions, rules, starts_at, ends_at, usage_limit, usage_count, per_customer_limit, status
  FROM promotion WHERE id = $1;

-- name: PromotionIDs :many
SELECT id FROM promotion ORDER BY id DESC;

-- name: InsertPromotion :exec
INSERT INTO promotion (site_id, id, name, promotion_type, starts_at, ends_at, usage_limit, per_customer_limit,
                       status, rules, actions)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- name: UpdatePromotion :execrows
UPDATE promotion SET name = $2, promotion_type = $3, starts_at = $4, ends_at = $5, usage_limit = $6,
       per_customer_limit = $7, status = $8, rules = $9, actions = $10
 WHERE id = $1;

-- name: Coupons :many
SELECT id, code, max_uses, usage_count, customer_id, status
  FROM coupon_code WHERE promotion_id = $1 ORDER BY code;

-- name: InsertCoupon :one
INSERT INTO coupon_code (site_id, id, promotion_id, code, max_uses, customer_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, code, max_uses, usage_count, customer_id, status;

-- name: SetCouponStatus :one
UPDATE coupon_code SET status = $3 WHERE id = $1 AND promotion_id = $2
RETURNING id, code, max_uses, usage_count, customer_id, status;

-- name: SetCartCoupon :exec
UPDATE cart SET coupon_code = $2 WHERE id = $1;

-- name: CouponPromotion :one
SELECT p.id, p.name, p.actions, p.rules, p.per_customer_limit, c.id AS coupon_id, c.code,
       c.customer_id AS coupon_customer
  FROM coupon_code c JOIN promotion p ON p.site_id = c.site_id AND p.id = c.promotion_id
 WHERE c.code = CAST(sqlc.arg(code)::text AS citext) AND c.status = 'active' AND (c.max_uses IS NULL OR c.usage_count < c.max_uses)
   AND p.status = 'active' AND (p.starts_at IS NULL OR p.starts_at <= now())
   AND (p.ends_at IS NULL OR p.ends_at > now())
   AND (p.usage_limit IS NULL OR p.usage_count < p.usage_limit);

-- name: Redemptions :one
SELECT count(*) FROM promotion_redemption WHERE promotion_id = $1 AND customer_id = $2;

-- name: AutomaticPromotions :many
SELECT p.id, p.name, p.actions, p.rules, p.per_customer_limit FROM promotion p
 WHERE p.status = 'active' AND (p.starts_at IS NULL OR p.starts_at <= now())
   AND (p.ends_at IS NULL OR p.ends_at > now())
   AND (p.usage_limit IS NULL OR p.usage_count < p.usage_limit)
   AND NOT EXISTS (SELECT 1 FROM coupon_code c WHERE c.site_id = p.site_id AND c.promotion_id = p.id)
 ORDER BY p.id;

-- name: CategoriesOfProducts :many
SELECT product_id, category_id FROM product_category WHERE product_id = ANY(@product_ids::uuid[]);

-- name: CountPromotionUse :execrows
UPDATE promotion SET usage_count = usage_count + 1
 WHERE id = $1 AND status = 'active' AND (usage_limit IS NULL OR usage_count < usage_limit);

-- name: CountCouponUse :execrows
UPDATE coupon_code SET usage_count = usage_count + 1
 WHERE id = $1 AND status = 'active' AND (max_uses IS NULL OR usage_count < max_uses);

-- name: InsertRedemption :exec
INSERT INTO promotion_redemption (site_id, id, promotion_id, customer_id, coupon_code_id, order_id)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: DeleteRedemptions :many
DELETE FROM promotion_redemption WHERE order_id = $1 RETURNING promotion_id, coupon_code_id;

-- name: UncountPromotionUse :exec
UPDATE promotion SET usage_count = greatest(usage_count - 1, 0) WHERE id = $1;

-- name: UncountCouponUse :exec
UPDATE coupon_code SET usage_count = greatest(usage_count - 1, 0) WHERE id = $1;
