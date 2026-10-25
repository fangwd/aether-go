package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BeginSite starts a transaction scoped to one site: row-level security
// limits every statement in it to siteID. The setting ends with the
// transaction, so it never leaks to the next user of a pooled connection.
func BeginSite(ctx context.Context, pool *pgxpool.Pool, siteID uuid.UUID) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err := ScopeToSite(ctx, tx, siteID); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// ScopeToSite limits the rest of the current transaction to one site.
func ScopeToSite(ctx context.Context, tx pgx.Tx, siteID uuid.UUID) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.site_id', $1::text, true)", siteID)
	return err
}

// InSite runs fn in a site-scoped transaction, committing when it returns nil.
func InSite(ctx context.Context, pool *pgxpool.Pool, siteID uuid.UUID, fn func(tx pgx.Tx, q *Queries) error) error {
	tx, err := BeginSite(ctx, pool, siteID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx, New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
