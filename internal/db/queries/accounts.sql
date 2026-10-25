-- name: InsertSession :one
INSERT INTO customer_session (site_id, id, customer_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4, now() + make_interval(days => @days::int)) RETURNING expires_at;

-- name: DeleteSession :exec
DELETE FROM customer_session WHERE id = $1;

-- name: DeleteSessionsOf :exec
DELETE FROM customer_session WHERE customer_id = $1;

-- name: RetireCodes :exec
UPDATE customer_code SET used_at = now() WHERE customer_id = $1 AND purpose = $2 AND used_at IS NULL;

-- name: InsertCode :exec
INSERT INTO customer_code (site_id, id, customer_id, purpose, code_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, now() + make_interval(mins => @minutes::int));

-- name: OpenCode :one
SELECT id, code_hash, attempts FROM customer_code
 WHERE customer_id = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
 ORDER BY created_at DESC LIMIT 1 FOR UPDATE;

-- name: CountCodeAttempt :exec
UPDATE customer_code SET attempts = attempts + 1 WHERE id = $1;

-- name: UseCode :exec
UPDATE customer_code SET used_at = now() WHERE id = $1;

-- name: AccountByEmail :one
SELECT id, password_hash::text AS password_hash, (email_verified_at IS NOT NULL)::bool AS verified
  FROM customer WHERE email = CAST(sqlc.arg(email)::text AS citext) AND password_hash IS NOT NULL;

-- name: UnverifiedAccountByEmail :one
SELECT id FROM customer
 WHERE email = CAST(sqlc.arg(email)::text AS citext) AND password_hash IS NOT NULL AND email_verified_at IS NULL;

-- name: InsertAccount :one
INSERT INTO customer (site_id, id, email, password_hash) VALUES ($1, $2, $3, $4)
ON CONFLICT (site_id, email) WHERE password_hash IS NOT NULL DO NOTHING RETURNING id;

-- name: MarkEmailVerified :exec
UPDATE customer SET email_verified_at = COALESCE(email_verified_at, now()) WHERE id = $1;

-- name: SetPassword :exec
UPDATE customer SET password_hash = $2 WHERE id = $1;

-- name: GuestProfileForUpdate :one
SELECT id FROM customer WHERE email = CAST(sqlc.arg(email)::text AS citext) AND password_hash IS NULL FOR UPDATE;

-- name: MoveOrders :exec
UPDATE "order" SET customer_id = @account WHERE customer_id = @guest;

-- name: MoveAddresses :exec
UPDATE customer_address SET customer_id = @account WHERE customer_id = @guest;

-- name: MoveCoupons :exec
UPDATE coupon_code SET customer_id = @account WHERE customer_id = @guest;

-- name: MoveRedemptions :exec
UPDATE promotion_redemption SET customer_id = @account WHERE customer_id = @guest;

-- name: DeleteCustomer :exec
DELETE FROM customer WHERE id = $1;
