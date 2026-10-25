package core_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"aether/internal/apitest"
	"aether/internal/db"
)

// Site isolation is enforced by Postgres row-level security. These tests run
// as aether_app, the role the API uses; a superuser would bypass every policy.

const (
	rlsViolation        = "42501"
	rlsForeignKeyFailed = "23503"
)

type rlsSite struct {
	id, productID uuid.UUID
}

// rlsSeedSite creates a site with one product, through the site's own transaction.
func rlsSeedSite(t *testing.T, pool *pgxpool.Pool) rlsSite {
	t.Helper()
	ctx := context.Background()
	site := rlsSite{id: uuid.Must(uuid.NewV7()), productID: uuid.Must(uuid.NewV7())}
	tx, err := db.BeginSite(ctx, pool, site.id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "INSERT INTO site (id, name, currency) VALUES ($1, 'Shop', 'USD')", site.id); err != nil {
		t.Fatal(err)
	}
	if err := rlsInsertProduct(tx, site.id, site.productID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return site
}

func rlsInsertProduct(tx pgx.Tx, siteID, id uuid.UUID) error {
	_, err := tx.Exec(context.Background(),
		"INSERT INTO product (site_id, id, slug, name, price) VALUES ($1, $2, $3, 'Tee', 10)", siteID, id, id.String())
	return err
}

func rlsSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func rlsBegin(t *testing.T, pool *pgxpool.Pool, siteID uuid.UUID) pgx.Tx {
	t.Helper()
	tx, err := db.BeginSite(context.Background(), pool, siteID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func rlsIDs(t *testing.T, tx pgx.Tx, sql string) []uuid.UUID {
	t.Helper()
	rows, err := tx.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func rlsCount(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSiteSeesOnlyItsOwnRows(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	a := rlsSeedSite(t, pool)
	b := rlsSeedSite(t, pool)

	tx := rlsBegin(t, pool, a.id)
	products := rlsIDs(t, tx, "SELECT id FROM product")
	sites := rlsIDs(t, tx, "SELECT id FROM site")

	if len(products) != 1 || products[0] != a.productID {
		t.Errorf("products %v, want [%s]", products, a.productID)
	}
	if len(sites) != 1 || sites[0] != a.id {
		t.Errorf("sites %v, want [%s]", sites, a.id)
	}
	for _, p := range products {
		if p == b.productID {
			t.Error("site A sees site B's product")
		}
	}
}

func TestUnsetSiteSeesNothing(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	rlsSeedSite(t, pool)

	products := rlsCount(t, pool, "SELECT count(*) FROM product")
	sites := rlsCount(t, pool, "SELECT count(*) FROM site")

	if products != 0 || sites != 0 {
		t.Errorf("(products, sites) = (%d, %d), want (0, 0)", products, sites)
	}
}

func TestCannotWriteRowsForAnotherSite(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	a := rlsSeedSite(t, pool)
	b := rlsSeedSite(t, pool)

	tx := rlsBegin(t, pool, a.id)
	err := rlsInsertProduct(tx, b.id, uuid.Must(uuid.NewV7()))

	if got := rlsSQLState(err); got != rlsViolation {
		t.Errorf("sqlstate %q (%v), want %s", got, err, rlsViolation)
	}
}

func TestCannotMoveARowToAnotherSite(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	a := rlsSeedSite(t, pool)
	b := rlsSeedSite(t, pool)

	tx := rlsBegin(t, pool, a.id)
	_, err := tx.Exec(context.Background(), "UPDATE product SET site_id = $1 WHERE id = $2", b.id, a.productID)

	if got := rlsSQLState(err); got != rlsViolation {
		t.Errorf("sqlstate %q (%v), want %s", got, err, rlsViolation)
	}
}

func TestCannotChangeAnotherSitesRows(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	a := rlsSeedSite(t, pool)
	b := rlsSeedSite(t, pool)
	ctx := context.Background()

	tx := rlsBegin(t, pool, a.id)
	updated, err := tx.Exec(ctx, "UPDATE product SET name = 'Stolen' WHERE id = $1", b.productID)
	if err != nil {
		t.Fatal(err)
	}
	deleted, err := tx.Exec(ctx, "DELETE FROM product WHERE id = $1", b.productID)
	if err != nil {
		t.Fatal(err)
	}

	if updated.RowsAffected() != 0 || deleted.RowsAffected() != 0 {
		t.Errorf("(updated, deleted) = (%d, %d), want (0, 0)", updated.RowsAffected(), deleted.RowsAffected())
	}
}

func TestCannotReferenceAnotherSitesRow(t *testing.T) {
	pool := apitest.NewDatabases(t).App
	a := rlsSeedSite(t, pool)
	b := rlsSeedSite(t, pool)

	// Foreign key checks ignore RLS; the composite (site_id, id) key is what
	// stops a site-A variant from pointing at site B's product.
	tx := rlsBegin(t, pool, a.id)
	_, err := tx.Exec(context.Background(),
		"INSERT INTO product_variant (site_id, id, product_id) VALUES ($1, $2, $3)", a.id, uuid.Must(uuid.NewV7()), b.productID)

	if got := rlsSQLState(err); got != rlsForeignKeyFailed {
		t.Errorf("sqlstate %q (%v), want %s", got, err, rlsForeignKeyFailed)
	}
}

func TestSiteSettingEndsWithItsTransaction(t *testing.T) {
	ctx := context.Background()
	app := apitest.NewDatabases(t).App
	// One connection, so the second query reuses the first transaction's.
	cfg := app.Config()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	a := rlsSeedSite(t, pool)

	tx, err := db.BeginSite(ctx, pool, a.id)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if visible := rlsCount(t, pool, "SELECT count(*) FROM product"); visible != 0 {
		t.Errorf("visible %d, want 0", visible)
	}
}

func TestWorkerRoleSeesEverySite(t *testing.T) {
	dbs := apitest.NewDatabases(t)
	rlsSeedSite(t, dbs.App)
	rlsSeedSite(t, dbs.App)

	if products := rlsCount(t, dbs.Worker, "SELECT count(*) FROM product"); products != 2 {
		t.Errorf("products %d, want 2", products)
	}
}
