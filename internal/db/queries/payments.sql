-- name: SetPaymentsEnabled :exec
UPDATE site SET payments_enabled = $1;

-- name: SetPaymentAccount :exec
UPDATE site SET payment_account_id = $1;

-- name: OrderToPay :one
SELECT grand_total, currency, payment_status, fulfilment_status FROM "order" WHERE id = $1 FOR UPDATE;

-- name: OpenPayment :one
SELECT provider_reference, (metadata->'client')::jsonb AS client FROM payment
 WHERE order_id = $1 AND provider = $2 AND status = 'open' AND amount = $3
 ORDER BY created_at DESC LIMIT 1;

-- name: PaymentAttempts :one
SELECT count(*) FROM payment WHERE order_id = $1;

-- name: InsertPayment :exec
INSERT INTO payment (site_id, id, order_id, currency, provider, provider_reference, amount, status, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, jsonb_build_object('client', @client::jsonb));

-- name: OrderToRefund :one
SELECT currency, grand_total, refunded_total, payment_status FROM "order" WHERE id = $1 FOR UPDATE;

-- name: RefundableQuantity :one
SELECT quantity - refunded_quantity FROM order_item WHERE id = $1 AND order_id = $2;

-- name: SucceededPayment :one
SELECT id, provider_reference FROM payment WHERE order_id = $1 AND status = 'succeeded'
 ORDER BY created_at DESC LIMIT 1;

-- name: InsertRefund :exec
INSERT INTO refund (site_id, id, order_id, currency, payment_id, provider, provider_reference, amount, reason,
                    status, metadata)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'pending', jsonb_build_object('items', @items::jsonb, 'subject', @subject::text));

-- name: SettleRefund :one
UPDATE refund SET status = $2 WHERE id = $1 AND status = 'pending'
RETURNING order_id, amount, metadata;

-- name: OrderPaymentStatusForUpdate :one
SELECT payment_status FROM "order" WHERE id = $1 FOR UPDATE;

-- name: AddRefunded :one
UPDATE "order" SET refunded_total = refunded_total + @amount::numeric,
       payment_status = CASE WHEN refunded_total + @amount::numeric >= grand_total THEN 'refunded' ELSE 'partially_refunded' END
 WHERE id = @id
RETURNING payment_status;

-- name: MarkItemRefunded :one
UPDATE order_item SET refunded_quantity = refunded_quantity + $2 WHERE id = $1 RETURNING variant_id;

-- name: MarkPaymentSucceeded :one
UPDATE payment SET status = 'succeeded' WHERE provider = $1 AND provider_reference = $2 RETURNING id;

-- name: MarkOrderPaid :exec
UPDATE "order" SET payment_status = 'paid' WHERE id = $1;

-- name: ClosePayment :exec
UPDATE payment SET status = $3 WHERE provider = $1 AND provider_reference = $2 AND status <> 'succeeded';

-- name: OpenPayments :many
SELECT id, provider, provider_reference FROM payment WHERE order_id = $1 AND status = ANY(@statuses::text[]);

-- name: SetPaymentStatus :exec
UPDATE payment SET status = $2 WHERE id = $1;
