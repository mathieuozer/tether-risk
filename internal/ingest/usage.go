package ingest

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/mozer/tether-risk/internal/chain/tron"
	"github.com/mozer/tether-risk/internal/store"
)

// TrackTronUsage adds this process's TronGrid requests to the shared daily
// count every few seconds until ctx ends, then once more
// (docs/DECISIONS.md D35). Every process that talks to TronGrid runs it, so
// the worker's budget sees what screens and the labeler spent too.
func TrackTronUsage(ctx context.Context, pg *sql.DB, log *slog.Logger) {
	flush := func(ctx context.Context) {
		n := tron.TakeRequests()
		if err := store.AddAPIUsage(ctx, pg, "trongrid", n); err != nil {
			log.Warn("tron usage not recorded", "requests", n, "error", err)
		}
	}
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				flush(context.WithoutCancel(ctx))
				return
			case <-tick.C:
				flush(ctx)
			}
		}
	}()
}

// FlushTronUsage records what is left, for a process about to exit.
func FlushTronUsage(ctx context.Context, pg *sql.DB, log *slog.Logger) {
	n := tron.TakeRequests()
	if err := store.AddAPIUsage(ctx, pg, "trongrid", n); err != nil {
		log.Warn("tron usage not recorded", "requests", n, "error", err)
	}
}
