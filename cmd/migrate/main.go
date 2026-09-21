// Command migrate applies the ClickHouse and PostgreSQL schema migrations.
//
// Safe to run repeatedly: applied migrations are recorded with a content hash
// and skipped, and an applied migration whose contents later change is a hard
// error rather than a silent schema drift.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mozer/tether-risk/internal/store"
)

func main() {
	var (
		only    = flag.String("only", "", "apply just one engine: postgres|clickhouse")
		verbose = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, *only, log); err != nil {
		log.Error("migration failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, only string, log *slog.Logger) error {
	if only == "" || only == "postgres" {
		db, err := store.OpenPostgres(ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := store.MigratePostgres(ctx, db, log); err != nil {
			return err
		}
	}

	if only == "" || only == "clickhouse" {
		db, err := store.OpenClickHouse(ctx)
		if err != nil {
			return err
		}
		defer db.Close()
		if err := store.MigrateClickHouse(ctx, db, log); err != nil {
			return err
		}
	}

	return nil
}
