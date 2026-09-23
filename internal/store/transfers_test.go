package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/mozer/tether-risk/internal/chain"
)

// These tests need the local stack (`make up`). They are skipped under
// `go test -short`, which is what `make test` runs.
//
// They run against dedicated `_test` databases, never the working ones. An
// earlier revision truncated the real tables, which silently destroyed
// locally ingested chain data whenever anyone ran `go test ./...` — and then
// produced a genuinely alarming debugging session, because a re-fetch after
// the wipe looks exactly like deduplication failing. Tests that destroy the
// developer's data are worse than tests that are slightly harder to set up.
func testDBs(t *testing.T) (ch, pg *sql.DB) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs ClickHouse and PostgreSQL; run `make up` then `make test-integration`")
	}
	ctx := context.Background()

	ch, pg = openTestDatabases(ctx, t)
	t.Cleanup(func() { ch.Close(); pg.Close() })

	truncateAll(t, ch, pg)
	t.Cleanup(func() { truncateAll(t, ch, pg) })
	return ch, pg
}

const testDBName = "tether_risk_test"

// openTestDatabases creates the isolated test databases if needed, migrates
// them, and returns connections to them.
func openTestDatabases(ctx context.Context, t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()

	// --- ClickHouse ---
	admin, err := OpenClickHouse(ctx)
	if err != nil {
		t.Skipf("clickhouse unavailable: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+testDBName); err != nil {
		admin.Close()
		t.Fatalf("create clickhouse test database: %v", err)
	}
	admin.Close()

	t.Setenv("CLICKHOUSE_DB", testDBName)
	ch, err := OpenClickHouse(ctx)
	if err != nil {
		t.Fatalf("open clickhouse test database: %v", err)
	}

	// --- PostgreSQL ---
	pgAdmin, err := OpenPostgres(ctx)
	if err != nil {
		ch.Close()
		t.Skipf("postgres unavailable: %v", err)
	}
	var exists bool
	if err := pgAdmin.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, testDBName).Scan(&exists); err != nil {
		pgAdmin.Close()
		ch.Close()
		t.Fatalf("check postgres test database: %v", err)
	}
	if !exists {
		// CREATE DATABASE cannot run inside a transaction, hence the plain Exec.
		if _, err := pgAdmin.ExecContext(ctx, "CREATE DATABASE "+testDBName); err != nil {
			pgAdmin.Close()
			ch.Close()
			t.Fatalf("create postgres test database: %v", err)
		}
	}
	pgAdmin.Close()

	t.Setenv("POSTGRES_DB", testDBName)
	pg, err := OpenPostgres(ctx)
	if err != nil {
		ch.Close()
		t.Fatalf("open postgres test database: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := MigratePostgres(ctx, pg, quiet); err != nil {
		ch.Close()
		pg.Close()
		t.Fatalf("migrate postgres test database: %v", err)
	}
	if err := MigrateClickHouse(ctx, ch, quiet); err != nil {
		ch.Close()
		pg.Close()
		t.Fatalf("migrate clickhouse test database: %v", err)
	}
	return ch, pg
}

func truncateAll(t *testing.T, ch, pg *sql.DB) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"transfers", "transfers_by_to", "edges", "edges_by_to"} {
		if _, err := ch.ExecContext(ctx, "TRUNCATE TABLE IF EXISTS "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	if _, err := pg.ExecContext(ctx, "TRUNCATE TABLE ingest_batches"); err != nil {
		t.Fatalf("truncate ingest_batches: %v", err)
	}
}

// sampleTransfers builds a small deterministic set: two transfers on one edge
// and one on another.
func sampleTransfers() []chain.Transfer {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	usd := func(v int64) *chain.Decimal {
		d := chain.NewDecimal(v, 0)
		return &d
	}
	return []chain.Transfer{
		{
			Chain: "tron", TxHash: "tx1", LogIndex: 0, BlockNumber: 100, BlockTime: at,
			FromAddress: "TAlice", ToAddress: "TBob", Asset: "USDT",
			RawValue: big.NewInt(1_000_000), USDValue: usd(100), PriceBasis: "pinned",
		},
		{
			Chain: "tron", TxHash: "tx2", LogIndex: 0, BlockNumber: 101, BlockTime: at.Add(time.Hour),
			FromAddress: "TAlice", ToAddress: "TBob", Asset: "USDT",
			RawValue: big.NewInt(2_000_000), USDValue: usd(200), PriceBasis: "pinned",
		},
		{
			Chain: "tron", TxHash: "tx3", LogIndex: 0, BlockNumber: 102, BlockTime: at.Add(2 * time.Hour),
			FromAddress: "TAlice", ToAddress: "TCarol", Asset: "USDT",
			RawValue: big.NewInt(5_000_000), USDValue: usd(500), PriceBasis: "pinned",
		},
	}
}

func edgeUSD(t *testing.T, ch *sql.DB, from, to string) string {
	t.Helper()
	var v string
	err := ch.QueryRowContext(context.Background(), `
		SELECT toString(sum(total_usd_value)) FROM edges
		WHERE chain = 'tron' AND from_address = ? AND to_address = ?`, from, to).Scan(&v)
	if err != nil {
		t.Fatalf("read edge %s->%s: %v", from, to, err)
	}
	return v
}

func edgeCount(t *testing.T, ch *sql.DB, from, to string) uint64 {
	t.Helper()
	var n uint64
	err := ch.QueryRowContext(context.Background(), `
		SELECT sum(transfer_count) FROM edges
		WHERE chain = 'tron' AND from_address = ? AND to_address = ?`, from, to).Scan(&n)
	if err != nil {
		t.Fatalf("read edge count %s->%s: %v", from, to, err)
	}
	return n
}

// TestEdgesInflateOnRawReinsert documents the trap that the whole
// deduplication design exists to prevent. It inserts the same transfers twice
// while bypassing WritePage, and asserts that `edges` really does double.
//
// If this test ever starts failing because the values no longer double, the
// engine's behaviour has changed and the deduplication in WritePage may be
// carrying less weight than its comments claim. Read it as an executable note
// on why that code is there, not as approval of the behaviour.
func TestEdgesInflateOnRawReinsert(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	transfers := sampleTransfers()

	// Insert twice, straight past the deduplication.
	for i := 0; i < 2; i++ {
		if err := w.insert(ctx, transfers); err != nil {
			t.Fatalf("raw insert %d: %v", i, err)
		}
	}

	// `transfers` collapses the duplicates (eventually).
	var distinct uint64
	if err := ch.QueryRowContext(ctx,
		`SELECT count() FROM (SELECT DISTINCT chain, tx_hash, log_index FROM transfers)`).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 3 {
		t.Errorf("distinct transfer keys = %d, want 3", distinct)
	}

	// `edges` does not. This is the silent corruption.
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "600" {
		t.Errorf("edge TAlice->TBob = %s, expected the inflated 600 "+
			"(2 x 300); if this changed, revisit docs/DECISIONS.md D2", got)
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 4 {
		t.Errorf("edge transfer_count = %d, expected the inflated 4 (2 x 2)", got)
	}

	// And the rebuild repairs it, which is the third defence.
	if err := w.RebuildEdges(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "300" {
		t.Errorf("after rebuild edge TAlice->TBob = %s, want the true 300", got)
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 2 {
		t.Errorf("after rebuild transfer_count = %d, want the true 2", got)
	}
}

// TestWritePageIsIdempotent is the positive case: SPEC.md §5 requires
// ingestion be idempotent and safely re-runnable.
func TestWritePageIsIdempotent(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	transfers := sampleTransfers()

	first, err := w.WritePage(ctx, "TAlice", "page-1", transfers)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if first.Inserted != 3 || first.Duplicates != 0 {
		t.Errorf("first write: inserted=%d duplicates=%d, want 3 and 0",
			first.Inserted, first.Duplicates)
	}

	// Replaying the identical page must do nothing at all.
	second, err := w.WritePage(ctx, "TAlice", "page-1", transfers)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.SkippedPage {
		t.Error("replaying a recorded page should be skipped by the ingest ledger")
	}
	if second.Inserted != 0 {
		t.Errorf("replay inserted %d rows; edges would now be inflated", second.Inserted)
	}

	// A different page key carrying the same transfers must still not
	// double-count: overlapping pages are normal in paginated APIs.
	third, err := w.WritePage(ctx, "TAlice", "page-2-overlapping", transfers)
	if err != nil {
		t.Fatalf("overlapping page: %v", err)
	}
	if third.Inserted != 0 {
		t.Errorf("overlapping page inserted %d rows, want 0", third.Inserted)
	}
	if third.Duplicates != 3 {
		t.Errorf("overlapping page duplicates = %d, want 3", third.Duplicates)
	}

	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "300" {
		t.Errorf("edge TAlice->TBob = %s, want 300 after three write attempts", got)
	}
	if got := edgeUSD(t, ch, "TAlice", "TCarol"); got != "500" {
		t.Errorf("edge TAlice->TCarol = %s, want 500", got)
	}
}

// Duplicates inside a single page must also be dropped: an upstream API can
// return the same event twice across a page boundary.
func TestWritePageDropsIntraPageDuplicates(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	transfers := sampleTransfers()
	withDupes := append(append([]chain.Transfer{}, transfers...), transfers[0], transfers[1])

	res, err := w.WritePage(ctx, "TAlice", "page-1", withDupes)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if res.Fetched != 5 {
		t.Errorf("fetched = %d, want 5", res.Fetched)
	}
	if res.Inserted != 3 {
		t.Errorf("inserted = %d, want 3; intra-page duplicates inflate edges too", res.Inserted)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "300" {
		t.Errorf("edge TAlice->TBob = %s, want 300", got)
	}
}

// The incremental materialized views and the deterministic rebuild must agree.
// The rebuild is ground truth; if they diverge, traversal is reading numbers
// that cannot be reproduced.
func TestRebuildMatchesIncremental(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	// Write across several pages, the way real ingestion does.
	all := sampleTransfers()
	for i, tr := range all {
		if _, err := w.WritePage(ctx, "TAlice", fmt.Sprintf("page-%d", i), []chain.Transfer{tr}); err != nil {
			t.Fatalf("write page %d: %v", i, err)
		}
	}

	before := snapshotEdges(t, ch, "edges")
	beforeByTo := snapshotEdges(t, ch, "edges_by_to")

	if err := w.RebuildEdges(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	after := snapshotEdges(t, ch, "edges")
	afterByTo := snapshotEdges(t, ch, "edges_by_to")

	if before != after {
		t.Errorf("incremental and rebuilt `edges` disagree:\n incremental: %s\n rebuilt:     %s", before, after)
	}
	if beforeByTo != afterByTo {
		t.Errorf("incremental and rebuilt `edges_by_to` disagree:\n incremental: %s\n rebuilt:     %s", beforeByTo, afterByTo)
	}
}

func snapshotEdges(t *testing.T, ch *sql.DB, table string) string {
	t.Helper()
	q := fmt.Sprintf(`
		SELECT groupArray(line) FROM (
			SELECT concat(from_address,'>',to_address,'/',asset,'=',
			              toString(sum(total_usd_value)),'x',toString(sum(transfer_count))) AS line
			FROM %s
			GROUP BY chain, from_address, to_address, asset
			ORDER BY from_address, to_address, asset
		)`, table)
	var lines []string
	if err := ch.QueryRowContext(context.Background(), q).Scan(&lines); err != nil {
		t.Fatalf("snapshot %s: %v", table, err)
	}
	return fmt.Sprint(lines)
}

// Inbound and outbound aggregates must describe the same flows. SPEC.md §1
// reports both directions, and a mismatch would mean one direction silently
// understates exposure.
func TestBothDirectionsAgree(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	if _, err := w.WritePage(ctx, "TAlice", "page-1", sampleTransfers()); err != nil {
		t.Fatal(err)
	}

	out, err := w.EdgeTotals(ctx, "tron", "TAlice", "outbound")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("outbound edges = %d, want 2", len(out))
	}
	// Ordered by value descending: TCarol (500) before TBob (300).
	if out[0].ToAddress != "TCarol" || out[0].TotalUSDValue.String() != "500" {
		t.Errorf("outbound[0] = %s/%s, want TCarol/500", out[0].ToAddress, out[0].TotalUSDValue)
	}
	if out[1].ToAddress != "TBob" || out[1].TotalUSDValue.String() != "300" {
		t.Errorf("outbound[1] = %s/%s, want TBob/300", out[1].ToAddress, out[1].TotalUSDValue)
	}

	in, err := w.EdgeTotals(ctx, "tron", "TBob", "inbound")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 1 {
		t.Fatalf("inbound edges for TBob = %d, want 1", len(in))
	}
	if in[0].FromAddress != "TAlice" || in[0].TotalUSDValue.String() != "300" {
		t.Errorf("inbound[0] = %s/%s, want TAlice/300", in[0].FromAddress, in[0].TotalUSDValue)
	}
}

// An adapter returning a malformed transfer is a bug. Storing it would corrupt
// aggregates in ways that are hard to trace back, so the write is refused.
func TestWritePageRejectsInvalidTransfers(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	bad := sampleTransfers()
	bad[1].ToAddress = ""

	if _, err := w.WritePage(ctx, "TAlice", "page-1", bad); err == nil {
		t.Fatal("expected a write of an invalid transfer to be refused")
	}

	var n uint64
	if err := ch.QueryRowContext(ctx, `SELECT count() FROM transfers`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows written despite validation failure; the batch must be all or nothing", n)
	}
}

// Unpriced transfers must contribute zero value while remaining countable.
// Treating "we do not know" as "zero" is how unknown exposure gets hidden,
// which SPEC.md §7 explicitly forbids.
func TestUnpricedTransfersAreCountedNotHidden(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	transfers := sampleTransfers()
	transfers[0].USDValue = nil
	transfers[0].PriceBasis = "unpriced"

	if _, err := w.WritePage(ctx, "TAlice", "page-1", transfers); err != nil {
		t.Fatal(err)
	}

	var unpriced, total uint64
	err := ch.QueryRowContext(ctx, `
		SELECT sum(unpriced_count), sum(transfer_count) FROM edges
		WHERE chain='tron' AND from_address='TAlice' AND to_address='TBob'`).Scan(&unpriced, &total)
	if err != nil {
		t.Fatal(err)
	}
	if unpriced != 1 {
		t.Errorf("unpriced_count = %d, want 1", unpriced)
	}
	if total != 2 {
		t.Errorf("transfer_count = %d, want 2", total)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "200" {
		t.Errorf("edge USD = %s, want 200 (the priced transfer only)", got)
	}
}

// A transfer written unpriced and later repriced is rewritten, and the edge
// views sum it twice. RepairEdges puts exactly that edge right, in both
// tables, without the empty window a full rebuild has (docs/DECISIONS.md
// D35).
func TestRepairEdgesAfterReprice(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	transfers := sampleTransfers()
	transfers[0].USDValue = nil
	transfers[0].PriceBasis = "unpriced"
	if _, err := w.WritePage(ctx, "TAlice", "page-1", transfers); err != nil {
		t.Fatal(err)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "200" {
		t.Fatalf("before reprice edge USD = %s, want 200", got)
	}

	repriced := sampleTransfers()[:1]
	if err := w.insert(ctx, repriced); err != nil {
		t.Fatal(err)
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 3 {
		t.Fatalf("the view should have counted the rewrite again: count %d, want 3", got)
	}

	k := EdgeKey{From: "TAlice", To: "TBob", Asset: repriced[0].Asset}
	if err := w.RepairEdges(ctx, "tron", []EdgeKey{k}); err != nil {
		t.Fatal(err)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "300" {
		t.Errorf("after repair edge USD = %s, want 300", got)
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 2 {
		t.Errorf("after repair transfer_count = %d, want 2", got)
	}
	var unpriced uint64
	var byTo string
	if err := ch.QueryRowContext(ctx, `
		SELECT sum(unpriced_count), toString(sum(total_usd_value)) FROM edges_by_to
		WHERE chain='tron' AND from_address='TAlice' AND to_address='TBob'`).Scan(&unpriced, &byTo); err != nil {
		t.Fatal(err)
	}
	if unpriced != 0 || byTo != "300" {
		t.Errorf("edges_by_to after repair: unpriced %d, usd %s; want 0 and 300", unpriced, byTo)
	}
}

// Two addresses' pages share the transfers between them. Written at the same
// time, each once found them missing and inserted them, and the edges
// counted them twice (docs/DECISIONS.md D37). The write lock makes the
// check and the insert one step.
func TestConcurrentPagesSharingTransfersCountOnce(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func(i int) {
			addr := []string{"TAlice", "TBob"}[i%2]
			_, err := w.WritePage(ctx, addr, fmt.Sprintf("page-%d", i), sampleTransfers())
			errs <- err
		}(i)
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 2 {
		t.Errorf("edge transfer_count = %d after concurrent writes, want 2", got)
	}
	if got := edgeUSD(t, ch, "TAlice", "TBob"); got != "300" {
		t.Errorf("edge USD = %s after concurrent writes, want 300", got)
	}
}

// The audit finds an edge counted twice, and an edge the views never
// received, and nothing else; the repair then agrees with the transfers.
func TestAuditEdgesFindsDriftAndRepairFixesIt(t *testing.T) {
	ch, pg := testDBs(t)
	w := NewTransferWriter(ch, pg)
	ctx := context.Background()

	if _, err := w.WritePage(ctx, "TAlice", "page-1", sampleTransfers()); err != nil {
		t.Fatal(err)
	}
	if keys, err := w.AuditEdges(ctx, "tron", 3); err != nil || len(keys) != 0 {
		t.Fatalf("clean edges audited as %v, %v", keys, err)
	}
	// Double count TAlice->TBob, and drop TAlice->TCarol from edges_by_to.
	if err := w.insert(ctx, sampleTransfers()[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.ExecContext(ctx, `DELETE FROM edges_by_to WHERE to_address = 'TCarol'`); err != nil {
		t.Fatal(err)
	}
	keys, err := w.AuditEdges(ctx, "tron", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("audit found %v, want the TBob and TCarol edges", keys)
	}
	if err := w.RepairEdges(ctx, "tron", keys); err != nil {
		t.Fatal(err)
	}
	if keys, err := w.AuditEdges(ctx, "tron", 3); err != nil || len(keys) != 0 {
		t.Fatalf("after repair the audit still finds %v, %v", keys, err)
	}
	if got := edgeCount(t, ch, "TAlice", "TBob"); got != 2 {
		t.Errorf("transfer_count = %d after repair, want 2", got)
	}
}
