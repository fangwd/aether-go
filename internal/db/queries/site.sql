-- name: GetSite :one
SELECT id, name, currency, timezone, prices_include_tax, country_code, region_code,
       payment_account_id, payments_enabled
  FROM site;

-- name: AnyOrders :one
SELECT EXISTS (SELECT 1 FROM "order");

-- name: UpsertSite :one
INSERT INTO site (id, name, currency, timezone, prices_include_tax, country_code, region_code)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, currency = EXCLUDED.currency, timezone = EXCLUDED.timezone,
    prices_include_tax = EXCLUDED.prices_include_tax, country_code = EXCLUDED.country_code,
    region_code = EXCLUDED.region_code
RETURNING id, name, currency, timezone, prices_include_tax, country_code, region_code,
          payment_account_id, payments_enabled;
