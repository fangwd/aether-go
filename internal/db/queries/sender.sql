-- name: Sender :one
SELECT domain, from_local, from_name, reply_to, status, dns_records, verified_at FROM email_sender;

-- name: SetSenderStatus :exec
UPDATE email_sender SET status = @status::text, checked_at = now(),
       verified_at = CASE WHEN @status::text = 'verified' THEN COALESCE(verified_at, now()) END;

-- name: UpsertSender :exec
INSERT INTO email_sender (site_id, domain, from_local, from_name, reply_to, dns_records)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (site_id) DO UPDATE SET domain = EXCLUDED.domain, from_local = EXCLUDED.from_local,
    from_name = EXCLUDED.from_name, reply_to = EXCLUDED.reply_to, dns_records = EXCLUDED.dns_records,
    status = CASE WHEN @changed::bool THEN 'pending' ELSE email_sender.status END,
    verified_at = CASE WHEN @changed::bool THEN NULL ELSE email_sender.verified_at END;

-- name: DeleteSender :execrows
DELETE FROM email_sender;
