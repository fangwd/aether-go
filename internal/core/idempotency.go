package core

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"aether/internal/db"
)

// idempotencyClaim replays the stored response when a client retries an
// unsafe request with the same Idempotency-Key. The key is claimed in the
// request's own transaction, so a failed request leaves nothing behind and
// may be retried. replay is nil on first use: do the work, then complete.
// fingerprint identifies the request the key was first used for; reusing a
// key for a different request is rejected.
func idempotencyClaim(ctx context.Context, q *db.Queries, siteID uuid.UUID, key, fingerprint string) (replay json.RawMessage, err error) {
	if len(key) < 1 || len(key) > 255 {
		return nil, badRequest("an Idempotency-Key header (1-255 characters) is required")
	}
	// A concurrent duplicate waits here until the first transaction ends.
	if _, err := q.ClaimIdempotencyKey(ctx, db.ClaimIdempotencyKeyParams{SiteID: siteID, Key: key, RequestPath: fingerprint}); err == nil {
		return nil, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	stored, err := q.StoredIdempotencyKey(ctx, key)
	if err != nil {
		return nil, err
	}
	if stored.RequestPath != fingerprint {
		return nil, unprocessable("this Idempotency-Key was used for a different request")
	}
	if stored.ResponseStatus == nil || stored.ResponseBody == nil {
		return nil, conflict("a request with this Idempotency-Key is still in progress")
	}
	return stored.ResponseBody, nil
}

func idempotencyComplete(ctx context.Context, q *db.Queries, key string, status int32, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return q.CompleteIdempotencyKey(ctx, db.CompleteIdempotencyKeyParams{Key: key, ResponseStatus: &status, ResponseBody: b})
}
