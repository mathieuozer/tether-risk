package tronindex

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Sink stores parsed blocks. WriteBlocks receives blocks in ascending order
// and must be idempotent per block: after a crash, the blocks since the last
// saved cursor are written again.
type Sink interface {
	WriteBlocks(ctx context.Context, blocks []BlockData) error
}

// CursorStore remembers the last block each run finished, by name.
type CursorStore interface {
	Load(ctx context.Context, name string) (uint64, bool, error)
	Save(ctx context.Context, name string, block uint64) error
}

// Indexer moves blocks from a Source to a Sink.
type Indexer struct {
	Source  Source
	Sink    Sink
	Cursors CursorStore
	Config  Config
	// Fetchers read blocks concurrently; the Sink still receives them in
	// order. Batch is how many blocks go to the Sink at once.
	Fetchers int
	Batch    int
	Poll     time.Duration // Tail's wait when caught up
	Log      *slog.Logger
	// OnBatch, if set, is called after each batch is written, with the
	// batch and how long it took to fetch and write. Measurement uses it.
	OnBatch func(blocks []BlockData, took time.Duration)
}

func (ix *Indexer) defaults() {
	if ix.Fetchers <= 0 {
		ix.Fetchers = 4
	}
	if ix.Batch <= 0 {
		ix.Batch = 50
	}
	if ix.Poll <= 0 {
		ix.Poll = 3 * time.Second
	}
	if ix.Log == nil {
		ix.Log = slog.Default()
	}
}

// Fetch reads and parses one block.
func (ix *Indexer) Fetch(ctx context.Context, n uint64) (BlockData, error) {
	b, err := ix.Source.Block(ctx, n)
	if err != nil {
		return BlockData{}, err
	}
	var infos []RawTxInfo
	if len(b.Transactions) > 0 {
		if infos, err = ix.Source.TxInfos(ctx, n); err != nil {
			return BlockData{}, err
		}
	}
	return ParseBlock(b, infos, ix.Config)
}

// Range indexes blocks from..to inclusive under a named cursor, resuming
// after the last block the cursor recorded.
func (ix *Indexer) Range(ctx context.Context, name string, from, to uint64) error {
	ix.defaults()
	next := from
	if last, ok, err := ix.Cursors.Load(ctx, name); err != nil {
		return err
	} else if ok && last+1 > next {
		next = last + 1
	}
	for next <= to {
		end := min(next+uint64(ix.Batch)-1, to)
		started := time.Now()
		blocks, err := ix.fetchBatch(ctx, next, end)
		if err != nil {
			return fmt.Errorf("%s: blocks %d-%d: %w", name, next, end, err)
		}
		if err := ix.Sink.WriteBlocks(ctx, blocks); err != nil {
			return fmt.Errorf("%s: write %d-%d: %w", name, next, end, err)
		}
		// The cursor moves only after the write, so a crash repeats blocks
		// rather than skipping them; the Sink makes the repeat harmless.
		if err := ix.Cursors.Save(ctx, name, end); err != nil {
			return err
		}
		if ix.OnBatch != nil {
			ix.OnBatch(blocks, time.Since(started))
		}
		next = end + 1
	}
	return nil
}

// fetchBatch reads blocks from..to concurrently and returns them in order.
func (ix *Indexer) fetchBatch(ctx context.Context, from, to uint64) ([]BlockData, error) {
	out := make([]BlockData, to-from+1)
	errs := make([]error, len(out))
	sem := make(chan struct{}, ix.Fetchers)
	var wg sync.WaitGroup
	for n := from; n <= to; n++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(n uint64) {
			defer wg.Done()
			defer func() { <-sem }()
			out[n-from], errs[n-from] = ix.Fetch(ctx, n)
		}(n)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", from+uint64(i), err)
		}
	}
	return out, nil
}

// Tail follows the source's head from the named cursor, or from start if the
// cursor is empty, until ctx ends.
func (ix *Indexer) Tail(ctx context.Context, name string, start uint64) error {
	ix.defaults()
	for {
		head, err := ix.Source.Head(ctx)
		if err != nil {
			ix.Log.Warn("head", "error", err)
		} else {
			from := start
			if last, ok, err := ix.Cursors.Load(ctx, name); err != nil {
				return err
			} else if ok {
				from = last + 1
			}
			if from <= head {
				if err := ix.Range(ctx, name, from, head); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					ix.Log.Warn("tail", "error", err)
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ix.Poll):
		}
	}
}

// Backfill indexes blocks from..to split into parts ranges run at once, each
// under its own cursor, newest range first so recent history completes
// earliest. A second run resumes every range where it stopped.
func (ix *Indexer) Backfill(ctx context.Context, from, to uint64, parts int) error {
	ix.defaults()
	if parts < 1 {
		parts = 1
	}
	span := (to - from + 1 + uint64(parts) - 1) / uint64(parts)
	var wg sync.WaitGroup
	errs := make([]error, parts)
	for p := parts - 1; p >= 0; p-- {
		lo := from + uint64(p)*span
		if lo > to {
			continue
		}
		hi := min(lo+span-1, to)
		wg.Add(1)
		go func(p int, lo, hi uint64) {
			defer wg.Done()
			errs[p] = ix.Range(ctx, fmt.Sprintf("backfill:%d-%d", lo, hi), lo, hi)
		}(p, lo, hi)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// MemoryCursors is a CursorStore held in memory, for tests and one-off runs.
type MemoryCursors struct {
	mu sync.Mutex
	m  map[string]uint64
}

func (c *MemoryCursors) Load(_ context.Context, name string) (uint64, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[name]
	return v, ok, nil
}

func (c *MemoryCursors) Save(_ context.Context, name string, block uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]uint64{}
	}
	c.m[name] = block
	return nil
}
