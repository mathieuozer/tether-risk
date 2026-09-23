// Package chain defines the normalised transfer shape and the per-chain
// adapter interface.
package chain

import (
	"context"
	"fmt"
	"hash/fnv"
	"math/big"
	"sort"
	"strconv"
	"time"
)

// Transfer is one normalised value movement: a native transfer or a token
// transfer, reduced to the same shape across chains.
type Transfer struct {
	Chain       string
	TxHash      string
	LogIndex    uint32
	BlockNumber uint64
	BlockTime   time.Time
	FromAddress string
	ToAddress   string
	Asset       string

	// RawValue is in the asset's smallest unit. Kept exact: a reviewer
	// auditing a score needs the on-chain number, not a derived one.
	RawValue *big.Int

	// USDValue is nil until pricing runs. Nil is meaningfully different from
	// zero and the two must not be conflated: zero means no value moved,
	// nil means we do not know what moved.
	USDValue *Decimal

	// PriceBasis records how USDValue was arrived at: "pinned",
	// "daily_close", or "unpriced". docs/PLAN.md F3 — native assets are
	// priced from a daily series, so individual transfers carry real error
	// and a score leaning on them is more weakly supported than one leaning
	// on stablecoins.
	PriceBasis string
}

// Key is the natural key SPEC.md §4 deduplicates on.
type Key struct {
	Chain    string
	TxHash   string
	LogIndex uint32
}

// Key returns the transfer's natural key.
func (t Transfer) Key() Key {
	return Key{Chain: t.Chain, TxHash: t.TxHash, LogIndex: t.LogIndex}
}

func (k Key) String() string {
	return fmt.Sprintf("%s/%s/%d", k.Chain, k.TxHash, k.LogIndex)
}

// Validate rejects transfers that would corrupt downstream aggregates. An
// adapter returning a malformed transfer is a bug worth failing on rather
// than a row worth storing.
func (t Transfer) Validate() error {
	switch {
	case t.Chain == "":
		return fmt.Errorf("transfer has no chain")
	case t.TxHash == "":
		return fmt.Errorf("transfer has no tx_hash")
	case t.FromAddress == "":
		return fmt.Errorf("%s: transfer has no from_address", t.Key())
	case t.ToAddress == "":
		return fmt.Errorf("%s: transfer has no to_address", t.Key())
	case t.Asset == "":
		return fmt.Errorf("%s: transfer has no asset", t.Key())
	case t.RawValue == nil:
		return fmt.Errorf("%s: transfer has no raw_value", t.Key())
	case t.RawValue.Sign() < 0:
		return fmt.Errorf("%s: negative raw_value %s", t.Key(), t.RawValue)
	case t.BlockTime.IsZero():
		return fmt.Errorf("%s: transfer has no block_time", t.Key())
	}
	return nil
}

// Cursor is an adapter's resumable position. TronGrid paginates by opaque
// fingerprint rather than block height, so this is a string rather than a
// number (SPEC.md §5 requires the cursor be persisted and resumable).
type Cursor struct {
	Value     string
	LastBlock uint64
	Done      bool
}

// AddressPage is one page of an address's history.
type AddressPage struct {
	Transfers []Transfer
	Next      Cursor

	// PageKey identifies this page of upstream data so a retry is
	// recognisable in the ingest ledger.
	PageKey string
}

// Adapter is implemented by each chain.
//
// SPEC.md §5 defines Head and FetchRange, which are block-range oriented. But
// §5 also specifies that ingestion is demand-driven per address, which a
// block-range interface cannot express. FetchAddress is therefore part of the
// interface too; FetchRange is retained for optional full-chain backfill.
// See docs/DECISIONS.md D3.
type Adapter interface {
	// Chain returns the chain identifier, e.g. "tron".
	Chain() string

	// Head returns the current chain head block number.
	Head(ctx context.Context) (uint64, error)

	// FetchRange returns all transfers in a block range. Used for backfill.
	FetchRange(ctx context.Context, from, to uint64) ([]Transfer, error)

	// FetchAddress returns one page of an address's transfer history.
	// Callers iterate until Next.Done.
	FetchAddress(ctx context.Context, address string, cur Cursor) (AddressPage, error)
}

// ContentKey digests which transfers a page holds, for its ingest-ledger
// key. A page's position alone is not enough: an address with one page has
// the same position on every fetch, so a later fetch that found new
// transfers was taken for a replay and skipped. From 2026-09-22 until D42,
// no single-page address recorded anything after its first fetch.
func ContentKey(ts []Transfer) string {
	keys := make([]string, len(ts))
	for i, t := range ts {
		keys[i] = t.TxHash + "/" + strconv.FormatUint(uint64(t.LogIndex), 10)
	}
	sort.Strings(keys)
	h := fnv.New64a()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	return strconv.FormatUint(h.Sum64(), 16)
}
