-- name: FindSession :one
SELECT s.id, c.id AS customer_id, c.email::text AS email,
       (c.email_verified_at IS NOT NULL)::bool AS verified,
       (s.expires_at < now() + make_interval(days => sqlc.arg(days)::int - 1))::bool AS renew
  FROM customer_session s JOIN customer c ON c.id = s.customer_id
 WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: RenewSession :exec
UPDATE customer_session SET expires_at = now() + make_interval(days => sqlc.arg(days)::int) WHERE id = $1;
