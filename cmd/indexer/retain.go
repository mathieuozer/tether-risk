package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/mozer/tether-risk/internal/store"
	"github.com/mozer/tether-risk/pkg/tronindex"
)

// prune deletes what is older than keep, for an index on a machine without
// room for the whole chain (D45). The block ledger goes first, so coverage
// never claims blocks whose transfers are gone; edges wholly older than the
// cutoff go with them, and edges straddling it are recomputed by the audit.
// Contract events are kept: they are few, and the blacklist needs them all.
func prune(ctx context.Context, db string, keep time.Duration, log *slog.Logger) error {
	if keep <= 0 {
		return fmt.Errorf("prune needs -keep, e.g. -keep 72h")
	}
	os.Setenv("CLICKHOUSE_DB", db)
	ch, err := store.OpenClickHouseBatch(ctx)
	if err != nil {
		return err
	}
	defer ch.Close()
	pg, err := store.OpenPostgres(ctx)
	if err != nil {
		return err
	}
	defer pg.Close()

	cutoff := time.Now().Add(-keep).UTC().Truncate(time.Second)
	for _, q := range []string{
		`DELETE FROM indexed_blocks WHERE chain = 'tron' AND block_time < ?`,
		`DELETE FROM transfers WHERE chain = 'tron' AND block_time < ?`,
		`DELETE FROM transfers_by_to WHERE chain = 'tron' AND block_time < ?`,
		`DELETE FROM edges WHERE chain = 'tron' AND last_seen < ?`,
		`DELETE FROM edges_by_to WHERE chain = 'tron' AND last_seen < ?`,
	} {
		if _, err := ch.ExecContext(ctx, q, cutoff); err != nil {
			return fmt.Errorf("%s: %w", q, err)
		}
	}
	w := store.NewTransferWriter(ch, pg)
	keys, err := w.AuditEdges(ctx, "tron", 20)
	if err != nil {
		return err
	}
	if err := w.RepairEdges(ctx, "tron", keys); err != nil {
		return err
	}
	log.Info("pruned", "before", cutoff, "edges_recomputed", len(keys))
	return nil
}

// diskGuard refuses writes while the disk holding path has less than min
// bytes free. The tail treats the refusal like any failed batch: it waits
// and tries again, and resumes from its cursor once room is made.
type diskGuard struct {
	tronindex.Sink
	path string
	min  uint64
}

func (g diskGuard) WriteBlocks(ctx context.Context, blocks []tronindex.BlockData) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(g.path, &st); err == nil {
		if free := st.Bavail * uint64(st.Bsize); free < g.min {
			return fmt.Errorf("only %.1f GB free on %s, under the %.0f GB floor; not writing",
				float64(free)/1e9, g.path, float64(g.min)/1e9)
		}
	}
	return g.Sink.WriteBlocks(ctx, blocks)
}
