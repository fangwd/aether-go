-- name: TaxClasses :many
SELECT id, code, name FROM tax_class ORDER BY code;

-- name: InsertTaxClass :one
INSERT INTO tax_class (site_id, id, code, name) VALUES ($1, $2, $3, $4) RETURNING id, code, name;

-- name: UpdateTaxClass :one
UPDATE tax_class SET code = $2, name = $3 WHERE id = $1 RETURNING id, code, name;

-- name: DeleteTaxRatesOfClass :exec
DELETE FROM tax_rate WHERE tax_class_id = $1;

-- name: DeleteTaxClass :execrows
DELETE FROM tax_class WHERE id = $1;

-- name: TaxRates :many
SELECT id, tax_class_id, country_code, region_code, rate, priority, valid_from, valid_until
  FROM tax_rate ORDER BY tax_class_id, country_code NULLS FIRST, region_code NULLS FIRST, priority;

-- name: InsertTaxRate :one
INSERT INTO tax_rate (site_id, id, tax_class_id, country_code, region_code, rate, priority, valid_from, valid_until)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, tax_class_id, country_code, region_code, rate, priority, valid_from, valid_until;

-- name: UpdateTaxRate :one
UPDATE tax_rate SET tax_class_id = $2, country_code = $3, region_code = $4, rate = $5, priority = $6,
       valid_from = $7, valid_until = $8
 WHERE id = $1
RETURNING id, tax_class_id, country_code, region_code, rate, priority, valid_from, valid_until;

-- name: DeleteTaxRate :execrows
DELETE FROM tax_rate WHERE id = $1;

-- name: TaxRatesInForce :many
SELECT tax_class_id, country_code, region_code, rate, priority FROM tax_rate
 WHERE (valid_from IS NULL OR valid_from <= now()) AND (valid_until IS NULL OR valid_until > now());

-- name: TaxClassByCode :one
SELECT id FROM tax_class WHERE code = $1;
