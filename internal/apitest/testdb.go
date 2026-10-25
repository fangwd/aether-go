// Package apitest runs the API against a real database for tests: each
// test gets its own database, cloned from a migrated template.
package apitest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"aether/migrations"
)

// adminURL is a role that can create databases on the test cluster; the
// cluster also needs the roles from db/bootstrap.sql.
func adminURL() string {
	if url := os.Getenv("AETHER_TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgresql://postgres:postgres@127.0.0.1:5433/postgres"
}

var (
	templateOnce sync.Once
	templateName string
	templateErr  error
)

// template is a database migrated to the current schema, named after the
// migrations' hash so a schema change makes a new one.
func template(ctx context.Context) (string, error) {
	templateOnce.Do(func() {
		h := sha256.New()
		_ = fs.WalkDir(migrations.FS, ".", func(path string, _ fs.DirEntry, _ error) error {
			if strings.HasSuffix(path, ".sql") {
				b, _ := fs.ReadFile(migrations.FS, path)
				h.Write(b)
			}
			return nil
		})
		templateName = "aether_test_" + hex.EncodeToString(h.Sum(nil))[:12]
		templateErr = createTemplate(ctx, templateName)
	})
	return templateName, templateErr
}

func createTemplate(ctx context.Context, name string) error {
	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		return err
	}
	defer admin.Close(ctx)
	// Test binaries run in parallel; one creates the template, the rest wait.
	if _, err := admin.Exec(ctx, "SELECT pg_advisory_lock(2384)"); err != nil {
		return err
	}
	defer admin.Exec(ctx, "SELECT pg_advisory_unlock(2384)") //nolint:errcheck
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil || exists {
		return err
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		return err
	}
	conn, err := sql.Open("pgx", databaseURL(name))
	if err != nil {
		return err
	}
	defer conn.Close()
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, conn, "."); err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE "+name)
		return err
	}
	return nil
}

func databaseURL(name string) string {
	cfg, _ := pgx.ParseConfig(adminURL())
	return fmt.Sprintf("postgresql://%s:%s@%s:%d/%s", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// Databases are one test's pools on its own database.
type Databases struct {
	// App acts as aether_app, so row-level security applies.
	App *pgxpool.Pool
	// Worker acts as aether_worker (bypasses RLS to find work across sites).
	Worker *pgxpool.Pool
	// Admin is a superuser, for arranging state behind the API.
	Admin *pgxpool.Pool
}

// NewDatabases clones the template into a fresh database dropped after the test.
func NewDatabases(t testing.TB) Databases {
	t.Helper()
	ctx := context.Background()
	tmpl, err := template(ctx)
	if err != nil {
		t.Fatalf("test template database: %v", err)
	}
	name := "aether_t_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.Connect(ctx, adminURL())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s TEMPLATE %s", name, tmpl)); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)

	dbs := Databases{App: pool(t, name, "aether_app"), Worker: pool(t, name, "aether_worker"), Admin: pool(t, name, "")}
	t.Cleanup(func() {
		dbs.App.Close()
		dbs.Worker.Close()
		dbs.Admin.Close()
		if admin, err := pgx.Connect(ctx, adminURL()); err == nil {
			_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE %s WITH (FORCE)", name))
			_ = admin.Close(ctx)
		}
	})
	return dbs
}

func pool(t testing.TB, name, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(databaseURL(name))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 8
	if role != "" {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET ROLE "+role)
			return err
		}
	}
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
