-- name: NextDueEvent :one
SELECT id, site_id, topic, payload, attempts FROM outbox_event
 WHERE published_at IS NULL AND (next_retry_at IS NULL OR next_retry_at <= now())
 ORDER BY created_at LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: PublishEvent :exec
-- Codes are secrets: none is kept once delivered.
UPDATE outbox_event SET published_at = now(), payload = payload - 'code' WHERE id = $1;

-- name: RetryEvent :exec
-- Exponential backoff; after max_attempts it is parked for good.
UPDATE outbox_event SET attempts = @attempts::int, last_error = @last_error::text,
       next_retry_at = CASE WHEN @attempts::int >= @max_attempts::int THEN 'infinity'
                            ELSE now() + make_interval(secs => power(2, @attempts::int)::double precision) END
 WHERE id = @id;

-- name: RefundAmount :one
SELECT amount FROM refund WHERE id = $1;

-- name: SenderOfSite :one
SELECT s.name AS site_name, e.domain, e.from_local, e.from_name, e.reply_to, e.status
  FROM site s LEFT JOIN email_sender e ON e.site_id = s.id;

-- name: DueUnpaidOrders :many
SELECT o.site_id, o.id FROM "order" o JOIN site s ON s.id = o.site_id
 WHERE s.payments_enabled AND o.payment_status = 'pending' AND o.fulfilment_status = 'unfulfilled'
   AND o.placed_at < now() - make_interval(mins => @minutes::int)
 ORDER BY o.placed_at LIMIT 100;

-- name: ExpireCarts :execrows
UPDATE cart SET status = 'expired' WHERE status = 'active' AND expires_at < now();

-- name: PurgeIdempotencyKeys :exec
DELETE FROM idempotency_key WHERE expires_at < now();

-- name: PurgeEvents :exec
DELETE FROM outbox_event WHERE published_at < now() - make_interval(days => @days::int);

-- name: PurgeSessions :exec
DELETE FROM customer_session WHERE expires_at < now();

-- name: PurgeCodes :exec
DELETE FROM customer_code WHERE expires_at < now() - interval '1 day';

-- name: PendingSenders :many
SELECT site_id, domain FROM email_sender
 WHERE status = 'pending' AND (checked_at IS NULL OR checked_at < now() - make_interval(mins => @minutes::int))
 LIMIT 100;
