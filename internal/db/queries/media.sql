-- name: InsertMedia :exec
INSERT INTO media (site_id, id, storage_provider, storage_key, mime_type, metadata)
VALUES ($1, $2, $3, $4, $5, jsonb_build_object('alt', sqlc.narg(alt)::text));
