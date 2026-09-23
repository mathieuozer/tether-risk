package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2" // registers the "clickhouse" driver
	_ "github.com/jackc/pgx/v5/stdlib"         // registers the "pgx" driver
)

// SPEC.md §2: no secrets in the repo, configuration via environment
// variables. Defaults here match docker-compose.yml so a local developer needs
// no environment at all, while a real deployment sets everything explicitly.

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// PostgresDSN builds the PostgreSQL connection string from the environment.
func PostgresDSN() string {
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		return dsn
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		env("POSTGRES_USER", "tether"),
		env("POSTGRES_PASSWORD", "localdev"),
		env("POSTGRES_HOST", "localhost"),
		env("POSTGRES_PORT", "5433"),
		env("POSTGRES_DB", "tether_risk"),
		env("POSTGRES_SSLMODE", "disable"),
	)
}

// ClickHouseDSN builds the ClickHouse connection string from the environment.
func ClickHouseDSN() string {
	if dsn := os.Getenv("CLICKHOUSE_DSN"); dsn != "" {
		return dsn
	}
	return fmt.Sprintf("clickhouse://%s:%s@%s:%s/%s?dial_timeout=10s&read_timeout=60s",
		env("CLICKHOUSE_USER", "tether"),
		env("CLICKHOUSE_PASSWORD", "localdev"),
		env("CLICKHOUSE_HOST", "localhost"),
		env("CLICKHOUSE_NATIVE_PORT", "9000"),
		env("CLICKHOUSE_DB", "tether_risk"),
	)
}

// OpenPostgres opens and verifies a PostgreSQL connection pool.
func OpenPostgres(ctx context.Context) (*sql.DB, error) {
	db, err := sql.Open("pgx", PostgresDSN())
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres (is `make up` running?): %w", err)
	}
	return db, nil
}

// OpenClickHouse opens and verifies a ClickHouse connection pool.
func OpenClickHouse(ctx context.Context) (*sql.DB, error) {
	return openClickHouse(ctx, ClickHouseDSN())
}

// OpenClickHouseBatch opens a pool for batch analytics, whose whole-table
// joins can outlast the 60 s read timeout meant for interactive screens:
// the poisoning join took 21 s alone and over 60 s beside a busy worker
// (docs/DECISIONS.md D32). A DSN from the environment is left as given.
func OpenClickHouseBatch(ctx context.Context) (*sql.DB, error) {
	dsn := ClickHouseDSN()
	if os.Getenv("CLICKHOUSE_DSN") == "" {
		dsn = strings.Replace(dsn, "read_timeout=60s", "read_timeout=15m", 1)
	}
	return openClickHouse(ctx, dsn)
}

// OpenClickHouseDatabase opens a pool on another database of the same
// server, such as the TRON index (docs/INDEXER_PLAN.md), beside the main one.
func OpenClickHouseDatabase(ctx context.Context, db string) (*sql.DB, error) {
	u, err := url.Parse(ClickHouseDSN())
	if err != nil {
		return nil, fmt.Errorf("clickhouse dsn: %w", err)
	}
	u.Path = "/" + db
	return openClickHouse(ctx, u.String())
}

func openClickHouse(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(time.Hour)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping clickhouse (is `make up` running?): %w", err)
	}
	return db, nil
}
