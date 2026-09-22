// Package storetest opens migrated PostgreSQL databases for tests in other
// packages.
//
// Each caller names its own database. `go test ./...` runs packages in
// parallel, and two packages migrating one database at once would race on
// the migration ledger.
package storetest

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"testing"

	"github.com/mozer/tether-risk/internal/store"
)

// Postgres returns a connection to the named test database, created and
// migrated if needed. It skips the test under -short or without a stack.
// The database is never the working one: name must end in "_test".
func Postgres(t *testing.T, name string) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("needs PostgreSQL; run `make up` then `make test-integration`")
	}
	if len(name) < 5 || name[len(name)-5:] != "_test" {
		t.Fatalf("test database %q must end in _test", name)
	}
	ctx := context.Background()

	admin, err := store.OpenPostgres(ctx)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	var exists bool
	if err := admin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		admin.Close()
		t.Fatalf("check test database: %v", err)
	}
	if !exists {
		// CREATE DATABASE cannot run inside a transaction, hence the plain Exec.
		if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
			admin.Close()
			t.Fatalf("create test database: %v", err)
		}
	}
	admin.Close()

	t.Setenv("POSTGRES_DB", name)
	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })

	if err := store.MigratePostgres(ctx, pg, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return pg
}
