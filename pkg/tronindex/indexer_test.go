package tronindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeSource serves blocks 0..head, each with one TRX transfer.
type fakeSource struct {
	mu   sync.Mutex
	head uint64
}

func (s *fakeSource) Head(context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.head, nil
}

func (s *fakeSource) Block(_ context.Context, n uint64) (RawBlock, error) {
	var b RawBlock
	b.BlockHeader.RawData.Number = n
	b.BlockHeader.RawData.Timestamp = int64(n) * 3000
	var tx RawTx
	tx.TxID = fmt.Sprintf("tx%d", n)
	value, _ := json.Marshal(map[string]any{"amount": 1_000_000 + n,
		"owner_address": "TZ8Ksz21Hk1tQuztCKCUJBRXStCav9uyjM", "to_address": "TWiZJrAmU9jgu64LqstWVnq7xNWHMqxGTS"})
	tx.RawData.Contract = append(tx.RawData.Contract, struct {
		Type      string `json:"type"`
		Parameter struct {
			Value json.RawMessage `json:"value"`
		} `json:"parameter"`
	}{Type: "TransferContract"})
	tx.RawData.Contract[0].Parameter.Value = value
	b.Transactions = []RawTx{tx}
	return b, nil
}

func (s *fakeSource) TxInfos(context.Context, uint64) ([]RawTxInfo, error) { return nil, nil }

// memSink records written blocks; it can be told to fail at one block.
type memSink struct {
	mu      sync.Mutex
	written []uint64
	failAt  uint64 // 0: never fail
	failed  bool
}

func (s *memSink) WriteBlocks(_ context.Context, bs []BlockData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range bs {
		if s.failAt != 0 && b.Number == s.failAt && !s.failed {
			s.failed = true
			return errors.New("disk full")
		}
	}
	for _, b := range bs {
		if len(b.Transfers) != 1 {
			return fmt.Errorf("block %d has %d transfers", b.Number, len(b.Transfers))
		}
		s.written = append(s.written, b.Number)
	}
	return nil
}

func newIndexer(src Source, sink Sink) *Indexer {
	return &Indexer{Source: src, Sink: sink, Cursors: &MemoryCursors{},
		Config: Config{NativeTRX: true}, Fetchers: 3, Batch: 7, Poll: 10 * time.Millisecond}
}

func TestRangeWritesInOrderAndResumesAfterFailure(t *testing.T) {
	sink := &memSink{failAt: 40}
	ix := newIndexer(&fakeSource{}, sink)
	ctx := context.Background()
	if err := ix.Range(ctx, "r", 10, 100); err == nil {
		t.Fatal("the failing write did not stop the range")
	}
	// A second run resumes after the last saved block: no block missing,
	// the failed batch written in full.
	if err := ix.Range(ctx, "r", 10, 100); err != nil {
		t.Fatal(err)
	}
	for i, n := range sink.written {
		if n != uint64(10+i) {
			t.Fatalf("written[%d] = %d; blocks must arrive in order with none missing", i, n)
		}
	}
	if len(sink.written) != 91 {
		t.Errorf("wrote %d blocks, want 91", len(sink.written))
	}
}

func TestBackfillCoversEveryBlockOnce(t *testing.T) {
	sink := &memSink{}
	ix := newIndexer(&fakeSource{}, sink)
	if err := ix.Backfill(context.Background(), 0, 999, 6); err != nil {
		t.Fatal(err)
	}
	seen := map[uint64]int{}
	for _, n := range sink.written {
		seen[n]++
	}
	for n := uint64(0); n < 1000; n++ {
		if seen[n] != 1 {
			t.Fatalf("block %d written %d times", n, seen[n])
		}
	}
}

func TestTailFollowsTheHead(t *testing.T) {
	src := &fakeSource{head: 20}
	sink := &memSink{}
	ix := newIndexer(src, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ix.Tail(ctx, "tail", 5) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		n := len(sink.written)
		sink.mu.Unlock()
		if n == 16 { // 5..20
			src.mu.Lock()
			src.head = 30
			src.mu.Unlock()
		}
		if n == 26 { // 5..30
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(sink.written) != 26 || sink.written[0] != 5 || sink.written[25] != 30 {
		t.Fatalf("tail wrote %v", sink.written)
	}
}
