// Command aether runs the commerce API.
//
//	aether serve    the HTTP API (default)
//	aether migrate  apply pending database migrations
//	aether worker   background jobs: emails, expiry, sending-domain checks
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"aether/internal/core"
	"aether/migrations"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	setupLogging(env("AETHER_LOG_FORMAT", "json"))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx)
	case "migrate":
		err = migrate(ctx)
	case "worker":
		err = work(ctx)
	default:
		err = fmt.Errorf("unknown command %q (expected serve, migrate or worker)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func setupLogging(format string) {
	var h slog.Handler = slog.NewJSONHandler(os.Stdout, nil)
	if format == "pretty" {
		h = slog.NewTextHandler(os.Stdout, nil)
	}
	slog.SetDefault(slog.New(h))
}

func serve(ctx context.Context) error {
	maxConns, err := envInt("AETHER_DB_MAX_CONNECTIONS", 20)
	if err != nil {
		return err
	}
	pool, err := connect(ctx, "AETHER_DATABASE_URL", maxConns)
	if err != nil {
		return err
	}
	defer pool.Close()

	keys, err := partnerKeys()
	if err != nil {
		return err
	}
	mail, err := mailer(ctx)
	if err != nil {
		return err
	}
	handler, err := core.New(core.Deps{
		Pool:        pool,
		PartnerKeys: keys,
		Mailer:      mail,
		Images:      imageStore(),
		Payments:    paymentGateway(),
	}).Handler()
	if err != nil {
		return err
	}

	// 2384 spells AETH on a phone keypad.
	srv := &http.Server{Addr: env("AETHER_LISTEN", "0.0.0.0:2384"), Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("shutting down")
	return nil
}

func work(ctx context.Context) error {
	pool, err := connect(ctx, "AETHER_WORKER_DATABASE_URL", 5)
	if err != nil {
		return err
	}
	defer pool.Close()
	mail, err := mailer(ctx)
	if err != nil {
		return err
	}
	hook, err := catalogHook()
	if err != nil {
		return err
	}
	w := &core.Worker{
		Pool:         pool,
		Mailer:       mail,
		Payments:     paymentGateway(),
		FallbackFrom: env("AETHER_EMAIL_FALLBACK_FROM", ""),
		CatalogHook:  hook,
	}
	slog.Info("worker started")
	w.Run(ctx)
	slog.Info("shutting down")
	return nil
}

// connect opens a pool from the URL in the named setting.
func connect(ctx context.Context, name string, maxConns int) (*pgxpool.Pool, error) {
	url, err := required(name)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = int32(maxConns)
	return pgxpool.NewWithConfig(ctx, cfg)
}

func migrate(ctx context.Context) error {
	url, err := required("AETHER_MIGRATE_DATABASE_URL")
	if err != nil {
		return err
	}
	conn, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer conn.Close()
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, conn, "."); err != nil {
		return err
	}
	slog.Info("migrations applied")
	return nil
}
