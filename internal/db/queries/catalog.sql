-- name: ProductsByIDs :many
SELECT id, slug, name, short_description, description, price, compare_at_price, cost_amount,
       status, tax_class_id, variant_axes, attributes, created_at, updated_at
  FROM product WHERE id = ANY(@ids::uuid[]);

-- name: VariantsByProducts :many
SELECT v.id, v.product_id, v.sku, v.barcode, v.name, v.attributes, v.price_extra, v.status,
       v.weight, v.weight_unit, v.length, v.width, v.height, v.dimension_unit,
       i.on_hand, i.reserved, i.available
  FROM product_variant v
  LEFT JOIN inventory i ON i.site_id = v.site_id AND i.variant_id = v.id
 WHERE v.product_id = ANY(@ids::uuid[])
 ORDER BY v.sort_order, v.id;

-- name: OptionGroupsByProducts :many
SELECT id, product_id, code, name, min_select, max_select
  FROM product_option_group WHERE product_id = ANY(@ids::uuid[])
 ORDER BY sort_order, id;

-- name: OptionValuesByProducts :many
SELECT v.id, v.group_id, v.code, v.label, v.price_extra, v.is_default
  FROM product_option_value v
  JOIN product_option_group g ON g.site_id = v.site_id AND g.id = v.group_id
 WHERE g.product_id = ANY(@ids::uuid[])
 ORDER BY v.sort_order, v.id;

-- name: CategoriesByProducts :many
SELECT pc.product_id, c.id, c.slug, c.name
  FROM product_category pc
  JOIN category c ON c.site_id = pc.site_id AND c.id = pc.category_id
 WHERE pc.product_id = ANY(@ids::uuid[])
 ORDER BY pc.is_primary DESC, pc.position, c.name;

-- name: MediaByProducts :many
SELECT pm.product_id, pm.media_id, pm.role, m.storage_provider, m.storage_key, COALESCE(m.metadata->>'alt', '')::text AS alt
  FROM product_media pm
  JOIN media m ON m.site_id = pm.site_id AND m.id = pm.media_id
 WHERE pm.product_id = ANY(@ids::uuid[])
 ORDER BY pm.position;

-- name: AdminProductIDs :many
SELECT id FROM product
 WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(cursor)::uuid IS NULL OR id < sqlc.narg(cursor))
 ORDER BY id DESC LIMIT @lim;

-- name: StorefrontProductIDs :many
SELECT p.id FROM product p
 WHERE p.status = 'active'
   AND (sqlc.narg(cursor)::uuid IS NULL OR p.id < sqlc.narg(cursor))
   AND (sqlc.narg(category)::text IS NULL OR EXISTS (
           SELECT 1 FROM product_category pc
             JOIN category c ON c.site_id = pc.site_id AND c.id = pc.category_id
            WHERE pc.product_id = p.id AND c.slug = sqlc.narg(category) AND c.status = 'active'))
   AND (sqlc.narg(q)::text IS NULL OR NOT EXISTS (
           SELECT 1 FROM unnest(regexp_split_to_array(lower(trim(sqlc.narg(q))), '\s+')) AS w(word)
            WHERE w.word <> ''
              AND strpos(lower(concat_ws(' ', p.name, p.short_description, p.description)), w.word) = 0))
 ORDER BY p.id DESC LIMIT @lim;

-- name: ActiveProductIDBySlug :one
SELECT id FROM product WHERE slug = $1 AND status = 'active';

-- name: InsertProduct :exec
INSERT INTO product (site_id, id, tax_class_id, slug, name, short_description, description, price,
                     compare_at_price, cost_amount, status, variant_axes, attributes, published_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, CASE WHEN $11::text = 'active' THEN now() END);

-- name: ProductForUpdate :one
SELECT slug, name, short_description, description, price, compare_at_price, cost_amount, status,
       tax_class_id, attributes
  FROM product WHERE id = $1 FOR UPDATE;

-- name: UpdateProduct :exec
UPDATE product SET slug = $2, name = $3, short_description = $4, description = $5, price = $6,
       compare_at_price = $7, cost_amount = $8, status = $9, tax_class_id = $10, attributes = $11,
       published_at = COALESCE(published_at, CASE WHEN $9::text = 'active' THEN now() END)
 WHERE id = $1;

-- name: ProductPriceForUpdate :one
SELECT price FROM product WHERE id = $1 FOR UPDATE;

-- name: MinActiveVariantExtra :one
SELECT (count(*) > 0)::bool AS has_active, COALESCE(min(price_extra), 0)::numeric AS extra
  FROM product_variant WHERE product_id = $1 AND status = 'active';

-- name: VariantsOfProduct :many
SELECT id, attributes FROM product_variant WHERE product_id = $1;

-- name: DeleteUnreferencedVariants :exec
DELETE FROM product_variant v
 WHERE v.product_id = $1 AND v.id = ANY(@ids::uuid[])
   AND NOT EXISTS (SELECT 1 FROM order_item o WHERE o.site_id = v.site_id AND o.variant_id = v.id)
   AND NOT EXISTS (SELECT 1 FROM cart_item c WHERE c.site_id = v.site_id AND c.variant_id = v.id)
   AND NOT EXISTS (SELECT 1 FROM inventory_transaction t WHERE t.site_id = v.site_id AND t.variant_id = v.id)
   AND NOT EXISTS (SELECT 1 FROM inventory_reservation r WHERE r.site_id = v.site_id AND r.variant_id = v.id);

-- name: DeactivateVariants :exec
UPDATE product_variant SET status = 'inactive', sku = NULL WHERE product_id = $1 AND id = ANY(@ids::uuid[]);

-- name: SetVariantAxes :exec
UPDATE product SET variant_axes = $2 WHERE id = $1;

-- name: UpsertVariant :exec
INSERT INTO product_variant (site_id, id, product_id, sku, barcode, name, attributes, price_extra, status,
                             sort_order, weight, weight_unit, length, width, height, dimension_unit)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (site_id, id) DO UPDATE SET
    sku = EXCLUDED.sku, barcode = EXCLUDED.barcode, name = EXCLUDED.name,
    attributes = EXCLUDED.attributes, price_extra = EXCLUDED.price_extra, status = EXCLUDED.status,
    sort_order = EXCLUDED.sort_order, weight = EXCLUDED.weight, weight_unit = EXCLUDED.weight_unit,
    length = EXCLUDED.length, width = EXCLUDED.width, height = EXCLUDED.height,
    dimension_unit = EXCLUDED.dimension_unit;

-- name: OptionGroupsOfProduct :many
SELECT id, code FROM product_option_group WHERE product_id = $1;

-- name: DeleteOptionGroupsExcept :exec
DELETE FROM product_option_group WHERE product_id = $1 AND NOT (id = ANY(@keep::uuid[]));

-- name: UpsertOptionGroup :exec
INSERT INTO product_option_group (site_id, id, product_id, code, name, min_select, max_select, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (site_id, id) DO UPDATE SET code = EXCLUDED.code, name = EXCLUDED.name,
    min_select = EXCLUDED.min_select, max_select = EXCLUDED.max_select, sort_order = EXCLUDED.sort_order;

-- name: OptionValuesOfGroup :many
SELECT id, code FROM product_option_value WHERE group_id = $1;

-- name: DeleteOptionValuesExcept :exec
DELETE FROM product_option_value WHERE group_id = $1 AND NOT (id = ANY(@keep::uuid[]));

-- name: UpsertOptionValue :exec
INSERT INTO product_option_value (site_id, id, group_id, code, label, price_extra, is_default, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (site_id, id) DO UPDATE SET code = EXCLUDED.code, label = EXCLUDED.label,
    price_extra = EXCLUDED.price_extra, is_default = EXCLUDED.is_default, sort_order = EXCLUDED.sort_order;

-- name: DeleteProductCategories :exec
DELETE FROM product_category WHERE product_id = $1;

-- name: InsertProductCategory :exec
INSERT INTO product_category (site_id, product_id, category_id, is_primary, position) VALUES ($1, $2, $3, $4, $5);

-- name: DeleteProductMedia :exec
DELETE FROM product_media WHERE product_id = $1;

-- name: InsertProductMedia :exec
INSERT INTO product_media (site_id, product_id, media_id, role, position) VALUES ($1, $2, $3, $4, $5);

-- name: Categories :many
SELECT id, parent_id, slug, name, description, status, sort_order FROM category
 WHERE NOT @active_only::bool OR status = 'active'
 ORDER BY sort_order, name;

-- name: InsertCategory :one
INSERT INTO category (site_id, id, parent_id, slug, name, description, status, sort_order)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, parent_id, slug, name, description, status, sort_order;

-- name: CategoryIsAncestor :one
WITH RECURSIVE ancestor AS (
    SELECT id, parent_id FROM category WHERE category.id = @parent_id::uuid
    UNION SELECT c.id, c.parent_id FROM category c JOIN ancestor a ON c.id = a.parent_id
)
SELECT EXISTS (SELECT 1 FROM ancestor a WHERE a.id = @category_id::uuid);

-- name: UpdateCategory :one
UPDATE category SET parent_id = $2, slug = $3, name = $4, description = $5, status = $6, sort_order = $7
 WHERE id = $1
RETURNING id, parent_id, slug, name, description, status, sort_order;

-- name: DeleteCategory :execrows
DELETE FROM category WHERE id = $1;
