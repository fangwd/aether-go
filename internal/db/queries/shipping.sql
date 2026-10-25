-- name: ZoneByID :one
SELECT id, code, name FROM shipping_zone WHERE id = $1;

-- name: ZoneRegions :many
SELECT country_code, region_code FROM shipping_zone_region WHERE zone_id = $1 ORDER BY country_code, region_code;

-- name: DeleteZoneRegions :exec
DELETE FROM shipping_zone_region WHERE zone_id = $1;

-- name: InsertZoneRegion :exec
INSERT INTO shipping_zone_region (site_id, zone_id, country_code, region_code) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;

-- name: ZoneIDs :many
SELECT id FROM shipping_zone ORDER BY code;

-- name: InsertZone :exec
INSERT INTO shipping_zone (site_id, id, code, name) VALUES ($1, $2, $3, $4);

-- name: UpdateZone :execrows
UPDATE shipping_zone SET code = $2, name = $3 WHERE id = $1;

-- name: DeleteZone :execrows
DELETE FROM shipping_zone WHERE id = $1;

-- name: ShippingMethods :many
SELECT id, code, name, carrier, kind, status, tax_class_id FROM shipping_method ORDER BY code;

-- name: InsertShippingMethod :exec
INSERT INTO shipping_method (site_id, id, code, name, carrier, kind, status, tax_class_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: UpdateShippingMethod :execrows
UPDATE shipping_method SET code = $2, name = $3, carrier = $4, kind = $5, status = $6, tax_class_id = $7 WHERE id = $1;

-- name: DeleteShippingMethod :execrows
DELETE FROM shipping_method WHERE id = $1;

-- name: ShippingRates :many
SELECT id, method_id, zone_id, min_order_amount, max_order_amount, price FROM shipping_rate
 ORDER BY method_id, zone_id, min_order_amount;

-- name: ShippingMethodKind :one
SELECT kind FROM shipping_method WHERE id = $1;

-- name: InsertShippingRate :one
INSERT INTO shipping_rate (site_id, id, method_id, zone_id, min_order_amount, max_order_amount, price)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, method_id, zone_id, min_order_amount, max_order_amount, price;

-- name: UpdateShippingRate :one
UPDATE shipping_rate SET method_id = $2, zone_id = $3, min_order_amount = $4, max_order_amount = $5, price = $6
 WHERE id = $1
RETURNING id, method_id, zone_id, min_order_amount, max_order_amount, price;

-- name: DeleteShippingRate :execrows
DELETE FROM shipping_rate WHERE id = $1;

-- name: HasShippingMethods :one
SELECT EXISTS (SELECT 1 FROM shipping_method WHERE status = 'active');

-- name: ActiveShippingMethods :many
SELECT id, code, name, carrier, kind, tax_class_id FROM shipping_method WHERE status = 'active' ORDER BY code;

-- name: RateTiersFor :many
SELECT r.method_id, r.min_order_amount, r.max_order_amount, r.price,
       max(CASE WHEN zr.region_code = '*' THEN 1 ELSE 2 END)::int AS specificity
  FROM shipping_rate r
  JOIN shipping_zone_region zr ON zr.site_id = r.site_id AND zr.zone_id = r.zone_id
 WHERE zr.country_code = @country_code AND (zr.region_code = '*' OR zr.region_code = @region_code::text)
 GROUP BY r.site_id, r.id;
