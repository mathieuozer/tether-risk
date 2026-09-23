// Package indexapi serves the TRON index over HTTP: an address's transfers,
// the kept contract events, and what the index covers (docs/INDEXER_PLAN.md).
//
//	GET /v1/status
//	GET /v1/accounts/{address}/transfers?asset=USDT,TRX&direction=in|out|all
//	    &min_timestamp=&max_timestamp=&limit=&cursor=
//	GET /v1/events?event=AddedBlackList&min_timestamp=&max_timestamp=&limit=&cursor=
//
// Timestamps are Unix milliseconds, as TronGrid's. Every answer carries the
// index's coverage, and says when the window asked for reaches before it:
// an index that starts today must not read as "this address had no history".
package indexapi

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mozer/tether-risk/internal/pricing"
	"github.com/mozer/tether-risk/pkg/tronaddr"
	"github.com/shopspring/decimal"
	"golang.org/x/time/rate"
)

const (
	defaultLimit = 50
	maxLimit     = 200
)

// Server answers from a ClickHouse database written by the indexer.
type Server struct {
	CH *sql.DB
	// Keys are the accepted API keys, sent as X-API-Key. Empty accepts
	// every request, for a server bound to localhost.
	Keys map[string]bool
	// PerKey limits each key's requests; zero means 10 a second, burst 20.
	PerKey rate.Limit
	Log    *slog.Logger

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	cov      Coverage
	covAt    time.Time
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.status)
	mux.HandleFunc("GET /v1/accounts/{address}/transfers", s.transfers)
	mux.HandleFunc("GET /v1/events", s.events)
	return s.guard(mux)
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if len(s.Keys) > 0 && !s.Keys[key] {
			writeError(w, http.StatusUnauthorized, "missing or unknown X-API-Key")
			return
		}
		if !s.limiter(key).Allow() {
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limiter(key string) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.limiters == nil {
		s.limiters = map[string]*rate.Limiter{}
	}
	l := s.limiters[key]
	if l == nil {
		per, burst := s.PerKey, 20
		if per == 0 {
			per = 10
		}
		l = rate.NewLimiter(per, burst)
		s.limiters[key] = l
	}
	return l
}

// Coverage is the block range the index holds.
type Coverage struct {
	FirstBlock uint64    `json:"first_block"`
	FirstTime  time.Time `json:"first_time"`
	LastBlock  uint64    `json:"last_block"`
	LastTime   time.Time `json:"last_time"`
	Blocks     uint64    `json:"blocks"`
	// Missing counts blocks between the first and the last that are not
	// indexed: a backfill in progress, or a gap to repair. Zero means the
	// range is complete.
	Missing uint64 `json:"missing"`
	LagSec  int64  `json:"lag_seconds"`
}

// coverage is cached for five seconds: every request reports it.
func (s *Server) coverage(ctx context.Context) (Coverage, error) {
	s.mu.Lock()
	if time.Since(s.covAt) < 5*time.Second {
		c := s.cov
		s.mu.Unlock()
		return c, nil
	}
	s.mu.Unlock()
	var c Coverage
	err := s.CH.QueryRowContext(ctx, `
		SELECT min(block), argMin(block_time, block), max(block), argMax(block_time, block), uniqExact(block)
		FROM indexed_blocks WHERE chain = 'tron'`).Scan(&c.FirstBlock, &c.FirstTime, &c.LastBlock, &c.LastTime, &c.Blocks)
	if err != nil {
		return c, err
	}
	if c.Blocks > 0 {
		c.Missing = c.LastBlock - c.FirstBlock + 1 - c.Blocks
		c.LagSec = int64(time.Since(c.LastTime).Seconds())
	}
	c.FirstTime, c.LastTime = c.FirstTime.UTC(), c.LastTime.UTC()
	s.mu.Lock()
	s.cov, s.covAt = c, time.Now()
	s.mu.Unlock()
	return c, nil
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	c, err := s.coverage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"coverage": c})
}

// Transfer is one value movement, as the API returns it.
type Transfer struct {
	TxHash     string  `json:"tx_hash"`
	LogIndex   uint32  `json:"log_index"`
	Block      uint64  `json:"block"`
	Timestamp  int64   `json:"timestamp"`
	From       string  `json:"from"`
	To         string  `json:"to"`
	Asset      string  `json:"asset"`
	Amount     string  `json:"amount"`
	RawValue   string  `json:"raw_value"`
	USDValue   *string `json:"usd_value"`
	PriceBasis string  `json:"price_basis"`
	Direction  string  `json:"direction"`
}

// Meta accompanies every list.
type Meta struct {
	Coverage Coverage `json:"coverage"`
	// Partial is set when the window asked for starts before the index
	// does: transfers before coverage.first_time are not known here.
	Partial bool   `json:"partial"`
	Next    string `json:"next_cursor,omitempty"`
}

type window struct {
	min, max time.Time
	limit    int
	after    *position
}

// position is a keyset cursor: lists are newest first, and the next page
// starts after the last row returned.
type position struct {
	Time time.Time
	Tx   string
	N    uint32
}

func (p position) encode() string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s|%d", p.Time.Unix(), p.Tx, p.N)))
}

func decodePosition(v string) (*position, error) {
	b, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		return nil, errors.New("bad cursor")
	}
	parts := strings.Split(string(b), "|")
	if len(parts) != 3 {
		return nil, errors.New("bad cursor")
	}
	sec, err1 := strconv.ParseInt(parts[0], 10, 64)
	n, err2 := strconv.ParseUint(parts[2], 10, 32)
	if err1 != nil || err2 != nil {
		return nil, errors.New("bad cursor")
	}
	return &position{Time: time.Unix(sec, 0).UTC(), Tx: parts[1], N: uint32(n)}, nil
}

func parseWindow(r *http.Request) (window, error) {
	q := r.URL.Query()
	win := window{limit: defaultLimit, max: time.Date(2105, 12, 31, 0, 0, 0, 0, time.UTC)}
	for name, dst := range map[string]*time.Time{"min_timestamp": &win.min, "max_timestamp": &win.max} {
		if v := q.Get(name); v != "" {
			ms, err := strconv.ParseInt(v, 10, 64)
			if err != nil || ms < 0 {
				return win, fmt.Errorf("%s must be Unix milliseconds", name)
			}
			*dst = time.UnixMilli(ms).UTC()
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return win, fmt.Errorf("limit must be 1 to %d", maxLimit)
		}
		win.limit = n
	}
	if v := q.Get("cursor"); v != "" {
		p, err := decodePosition(v)
		if err != nil {
			return win, err
		}
		win.after = p
	}
	return win, nil
}

func (s *Server) transfers(w http.ResponseWriter, r *http.Request) {
	addr, err := tronaddr.Normalise(r.PathValue("address"))
	if err != nil || addr == "" {
		writeError(w, http.StatusBadRequest, "not a TRON address")
		return
	}
	win, err := parseWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	direction := q.Get("direction")
	if direction == "" {
		direction = "all"
	}
	if direction != "all" && direction != "in" && direction != "out" {
		writeError(w, http.StatusBadRequest, "direction must be in, out or all")
		return
	}
	var assets []string
	if v := q.Get("asset"); v != "" {
		for _, a := range strings.Split(v, ",") {
			assets = append(assets, strings.ToUpper(strings.TrimSpace(a)))
		}
	}

	// A self-transfer is returned once, as "out".
	cond, cargs := filters(win, assets)
	var branches []string
	var args []any
	branch := func(table, column, dir, extra string, extraArgs ...any) {
		branches = append(branches, fmt.Sprintf(`
			SELECT tx_hash, log_index, block_number, block_time, from_address, to_address,
			       asset, toString(raw_value), toString(usd_value), price_basis, '%s' AS direction
			FROM %s FINAL WHERE chain = 'tron' AND %s = ?%s%s`, dir, table, column, extra, cond))
		args = append(append(append(args, addr), extraArgs...), cargs...)
	}
	if direction != "in" {
		branch("transfers", "from_address", "out", "")
	}
	if direction != "out" {
		branch("transfers_by_to", "to_address", "in", " AND from_address != ?", addr)
	}
	query := "SELECT * FROM (" + strings.Join(branches, " UNION ALL ") + ")" +
		fmt.Sprintf(" ORDER BY block_time DESC, tx_hash DESC, log_index DESC LIMIT %d", win.limit+1)

	rows, err := s.CH.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	out := []Transfer{}
	var last position
	var more bool
	for rows.Next() {
		if len(out) == win.limit {
			more = true // the extra row only says there is a next page
			break
		}
		var t Transfer
		var at time.Time
		var usd sql.NullString
		if err := rows.Scan(&t.TxHash, &t.LogIndex, &t.Block, &at, &t.From, &t.To,
			&t.Asset, &t.RawValue, &usd, &t.PriceBasis, &t.Direction); err != nil {
			s.fail(w, err)
			return
		}
		t.Timestamp = at.UnixMilli()
		t.Amount = amount(t.Asset, t.RawValue)
		if usd.Valid && usd.String != "" && usd.String != "\\N" {
			t.USDValue = &usd.String
		}
		out = append(out, t)
		last = position{Time: at.UTC(), Tx: t.TxHash, N: t.LogIndex}
	}
	if err := rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	meta, err := s.meta(r.Context(), win)
	if err != nil {
		s.fail(w, err)
		return
	}
	if more {
		meta.Next = last.encode()
	}
	writeJSON(w, map[string]any{"data": out, "meta": meta})
}

func filters(win window, assets []string) (string, []any) {
	var b strings.Builder
	var args []any
	if len(assets) > 0 {
		b.WriteString(" AND asset IN (?)")
		args = append(args, assets)
	}
	b.WriteString(" AND block_time >= ? AND block_time <= ?")
	args = append(args, win.min, win.max)
	if win.after != nil {
		b.WriteString(" AND (block_time, tx_hash, log_index) < (?, ?, ?)")
		args = append(args, win.after.Time, win.after.Tx, win.after.N)
	}
	return b.String(), args
}

func (s *Server) meta(ctx context.Context, win window) (Meta, error) {
	c, err := s.coverage(ctx)
	if err != nil {
		return Meta{}, err
	}
	return Meta{Coverage: c, Partial: c.Blocks == 0 || win.min.Before(c.FirstTime) || c.Missing > 0}, nil
}

// Event is one kept contract event. Address is decoded for the blacklist
// events, whose one argument is the address.
type Event struct {
	Event     string   `json:"event"`
	Contract  string   `json:"contract"`
	TxHash    string   `json:"tx_hash"`
	Position  uint32   `json:"position"`
	Block     uint64   `json:"block"`
	Timestamp int64    `json:"timestamp"`
	Address   string   `json:"address,omitempty"`
	Topics    []string `json:"topics"`
	Data      string   `json:"data"`
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	win, err := parseWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query := `SELECT event, contract, tx_hash, position, block_number, block_time, topics, data
		FROM contract_events FINAL WHERE chain = 'tron' AND block_time >= ? AND block_time <= ?`
	args := []any{win.min, win.max}
	if v := r.URL.Query().Get("event"); v != "" {
		query += " AND event = ?"
		args = append(args, v)
	}
	if win.after != nil {
		query += " AND (block_time, tx_hash, position) < (?, ?, ?)"
		args = append(args, win.after.Time, win.after.Tx, win.after.N)
	}
	query += fmt.Sprintf(" ORDER BY block_time DESC, tx_hash DESC, position DESC LIMIT %d", win.limit+1)
	rows, err := s.CH.QueryContext(r.Context(), query, args...)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	out := []Event{}
	var last position
	var more bool
	for rows.Next() {
		if len(out) == win.limit {
			more = true
			break
		}
		var e Event
		var at time.Time
		if err := rows.Scan(&e.Event, &e.Contract, &e.TxHash, &e.Position, &e.Block, &at, &e.Topics, &e.Data); err != nil {
			s.fail(w, err)
			return
		}
		e.Timestamp = at.UnixMilli()
		if strings.HasSuffix(e.Event, "BlackList") || e.Event == "DestroyedBlackFunds" {
			e.Address = wordAddress(e.Data)
		}
		out = append(out, e)
		last = position{Time: at.UTC(), Tx: e.TxHash, N: e.Position}
	}
	if err := rows.Err(); err != nil {
		s.fail(w, err)
		return
	}
	meta, err := s.meta(r.Context(), win)
	if err != nil {
		s.fail(w, err)
		return
	}
	if more {
		meta.Next = last.encode()
	}
	writeJSON(w, map[string]any{"data": out, "meta": meta})
}

// wordAddress reads the address in an event's first 32-byte data word.
func wordAddress(data string) string {
	data = strings.TrimPrefix(data, "0x")
	if len(data) < 64 {
		return ""
	}
	a, err := tronaddr.HexToBase58("41" + data[24:64])
	if err != nil {
		return ""
	}
	return a
}

func amount(asset, raw string) string {
	d, ok := pricing.Decimals(asset)
	if !ok {
		return ""
	}
	v, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return ""
	}
	return decimal.NewFromBigInt(v, -d).String()
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.Log.Error("index api", "error", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
