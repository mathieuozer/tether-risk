// Package store holds ClickHouse and PostgreSQL access.
//
// Migrations are numbered SQL files applied in filename order and recorded in
// a ledger table so re-running is safe. SPEC.md §11 asks for boring,
// inspectable code: a reviewer should be able to read the schema as SQL and
// the runner in one sitting.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

//go:embed migrations/postgres/*.sql migrations/clickhouse/*.sql
var migrationFS embed.FS

// Migration is one numbered SQL file.
type Migration struct {
	Name string // e.g. "001_labels.sql"
	SQL  string
	Hash string // SHA-256 of SQL, so silent edits to applied migrations are caught
}

// LoadMigrations reads the embedded migrations for an engine ("postgres" or
// "clickhouse") in filename order.
func LoadMigrations(engine string) ([]Migration, error) {
	dir := "migrations/" + engine
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read %s migrations: %w", engine, err)
	}

	var out []Migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile(dir + "/" + e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		out = append(out, Migration{
			Name: e.Name(),
			SQL:  string(b),
			Hash: hex.EncodeToString(sum[:]),
		})
	}
	// Filename order is the apply order. Explicit sort rather than trusting
	// ReadDir, because apply order changing between platforms would be a
	// genuinely nasty bug.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// applier abstracts the two engines, which differ in how they record applied
// migrations and whether they can run DDL in a transaction.
type applier interface {
	ensureLedger(ctx context.Context) error
	applied(ctx context.Context) (map[string]string, error)
	apply(ctx context.Context, m Migration) error
	engine() string
}

func runMigrations(ctx context.Context, a applier, log *slog.Logger) error {
	migrations, err := LoadMigrations(a.engine())
	if err != nil {
		return err
	}
	if err := a.ensureLedger(ctx); err != nil {
		return fmt.Errorf("%s: ensure ledger: %w", a.engine(), err)
	}
	done, err := a.applied(ctx)
	if err != nil {
		return fmt.Errorf("%s: read ledger: %w", a.engine(), err)
	}

	var pending int
	for _, m := range migrations {
		if prevHash, ok := done[m.Name]; ok {
			// An applied migration whose contents changed means the database
			// and the repository disagree about the schema. Failing here is
			// the whole point of storing the hash.
			if prevHash != m.Hash {
				return fmt.Errorf(
					"%s: migration %s was applied with different contents (recorded %s, now %s); "+
						"edit a new migration rather than an applied one",
					a.engine(), m.Name, prevHash[:12], m.Hash[:12])
			}
			continue
		}
		log.Info("applying migration", "engine", a.engine(), "name", m.Name)
		if err := a.apply(ctx, m); err != nil {
			return fmt.Errorf("%s: apply %s: %w", a.engine(), m.Name, err)
		}
		pending++
	}

	if pending == 0 {
		log.Info("schema up to date", "engine", a.engine(), "migrations", len(migrations))
	} else {
		log.Info("migrations applied", "engine", a.engine(), "applied", pending)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

type pgApplier struct{ db *sql.DB }

func (p pgApplier) engine() string { return "postgres" }

func (p pgApplier) ensureLedger(ctx context.Context) error {
	_, err := p.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			hash       TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	return err
}

func (p pgApplier) applied(ctx context.Context) (map[string]string, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT name, hash FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, err
		}
		out[name] = hash
	}
	return out, rows.Err()
}

func (p pgApplier) apply(ctx context.Context, m Migration) error {
	// PostgreSQL runs DDL transactionally, so a failed migration leaves no
	// half-applied schema behind. The migration files carry their own
	// BEGIN/COMMIT; running them as one statement batch preserves that.
	if _, err := p.db.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, hash) VALUES ($1, $2)`, m.Name, m.Hash)
	return err
}

// ---------------------------------------------------------------------------
// ClickHouse
// ---------------------------------------------------------------------------

type chApplier struct{ db *sql.DB }

func (c chApplier) engine() string { return "clickhouse" }

func (c chApplier) ensureLedger(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       String,
			hash       String,
			applied_at DateTime DEFAULT now()
		) ENGINE = ReplacingMergeTree(applied_at)
		ORDER BY name`)
	return err
}

func (c chApplier) applied(ctx context.Context) (map[string]string, error) {
	// FINAL collapses the ReplacingMergeTree so a re-applied name does not
	// appear twice.
	rows, err := c.db.QueryContext(ctx, `SELECT name, hash FROM schema_migrations FINAL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, err
		}
		out[name] = hash
	}
	return out, rows.Err()
}

func (c chApplier) apply(ctx context.Context, m Migration) error {
	// ClickHouse has no transactional DDL and its driver rejects multiple
	// statements in one Exec, so statements are split and applied in order. A
	// failure part-way leaves the schema partly applied; the migration is not
	// recorded, so a re-run resumes. Every statement is written to be
	// idempotent (IF NOT EXISTS) for exactly this reason.
	for _, stmt := range splitSQL(m.SQL) {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("statement %q: %w", truncate(stmt, 80), err)
		}
	}
	_, err := c.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, hash) VALUES (?, ?)`, m.Name, m.Hash)
	return err
}

// splitSQL splits a migration file into statements on semicolons, ignoring
// semicolons inside line comments. The migration files are ours and contain no
// string literals with semicolons, so this stays deliberately simple rather
// than growing into a SQL parser.
func splitSQL(s string) []string {
	var out []string
	var cur strings.Builder

	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") || trimmed == "" {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmt := strings.TrimSpace(cur.String())
			stmt = strings.TrimSuffix(stmt, ";")
			if s := strings.TrimSpace(stmt); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// MigratePostgres applies the PostgreSQL migrations.
func MigratePostgres(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return runMigrations(ctx, pgApplier{db: db}, log)
}

// MigrateClickHouse applies the ClickHouse migrations.
func MigrateClickHouse(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	return runMigrations(ctx, chApplier{db: db}, log)
}
