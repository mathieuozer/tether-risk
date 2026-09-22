package labels

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Store persists labels with snapshot versioning.
//
// docs/DECISIONS.md D4: labels are slowly-changing-dimension type 2. A row is
// valid over a half-open range of snapshots, so resolving at snapshot S always
// returns what was known at S — which is what makes a score from last week
// reproducible today (SPEC.md §2).
type Store struct{ pg *sql.DB }

func NewStore(pg *sql.DB) *Store { return &Store{pg: pg} }

// Snapshot identifies an immutable version of the label set.
type Snapshot struct {
	ID           int64
	CreatedAt    time.Time
	SealedAt     *time.Time
	SourceCounts map[string]int
	Notes        string
}

// OpenSnapshot creates a new snapshot to write into.
func (s *Store) OpenSnapshot(ctx context.Context, notes string) (int64, error) {
	var id int64
	err := s.pg.QueryRowContext(ctx,
		`INSERT INTO label_snapshots (notes) VALUES ($1) RETURNING id`, notes).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("open snapshot: %w", err)
	}
	return id, nil
}

// SealSnapshot closes a snapshot and records per-source counts.
//
// SPEC.md §6 acceptance requires label counts per source be reported. They are
// computed and stored at seal time rather than counted on demand, so the
// numbers attached to a snapshot describe that snapshot rather than the
// database's current state.
func (s *Store) SealSnapshot(ctx context.Context, id int64) (map[string]int, error) {
	counts, err := s.CountsBySource(ctx, id)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(counts)
	if err != nil {
		return nil, err
	}
	if _, err := s.pg.ExecContext(ctx,
		`UPDATE label_snapshots SET sealed_at = now(), source_counts = $2 WHERE id = $1`,
		id, raw); err != nil {
		return nil, fmt.Errorf("seal snapshot: %w", err)
	}
	return counts, nil
}

// LatestSealedSnapshot returns the most recent sealed snapshot.
//
// Scoring resolves against a sealed snapshot, never an open one: an open
// snapshot is still being written, so two runs against it could legitimately
// differ and SPEC.md §2's determinism guarantee would not hold.
func (s *Store) LatestSealedSnapshot(ctx context.Context) (int64, error) {
	var id int64
	err := s.pg.QueryRowContext(ctx,
		`SELECT id FROM label_snapshots WHERE sealed_at IS NOT NULL ORDER BY id DESC LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no sealed label snapshot exists; run the labeler before scoring")
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertResult reports what an ingestion run changed.
type UpsertResult struct {
	Inserted  int
	Updated   int
	Unchanged int
}

// Upsert writes labels into a snapshot.
//
// For each (chain, address, source):
//   - no current row      -> insert, valid from this snapshot
//   - current row differs -> close the old row at this snapshot, insert the new
//   - current row matches -> leave it alone
//
// Leaving unchanged rows untouched is what keeps history meaningful. Closing
// and reopening an identical row every daily run would produce a version
// history where nothing can be distinguished from anything else.
func (s *Store) Upsert(ctx context.Context, snapshotID int64, in []Label) (UpsertResult, error) {
	var res UpsertResult

	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// Deterministic order so a run's writes are reproducible and deadlocks
	// between concurrent ingesters are avoided.
	labels := append([]Label(nil), in...)
	sort.Slice(labels, func(i, j int) bool {
		a, b := labels[i], labels[j]
		if a.Chain != b.Chain {
			return a.Chain < b.Chain
		}
		if a.Address != b.Address {
			return a.Address < b.Address
		}
		return a.Source < b.Source
	})

	for _, l := range labels {
		evidence, err := json.Marshal(l.Evidence)
		if err != nil {
			return res, fmt.Errorf("marshal evidence for %s: %w", l.Address, err)
		}

		var (
			curID       int64
			curEntity   string
			curCategory string
			curConf     float64
		)
		err = tx.QueryRowContext(ctx, `
			SELECT id, entity, category, confidence FROM labels
			WHERE chain = $1 AND address = $2 AND source = $3 AND valid_to_snapshot IS NULL`,
			l.Chain, l.Address, l.Source).Scan(&curID, &curEntity, &curCategory, &curConf)

		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO labels
					(chain, address, entity, category, confidence, source, evidence, valid_from_snapshot)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				l.Chain, l.Address, l.Entity, l.Category, l.Confidence, l.Source, evidence, snapshotID,
			); err != nil {
				return res, fmt.Errorf("insert label %s/%s: %w", l.Address, l.Source, err)
			}
			res.Inserted++

		case err != nil:
			return res, fmt.Errorf("read current label %s/%s: %w", l.Address, l.Source, err)

		case curEntity == l.Entity && curCategory == l.Category && curConf == l.Confidence:
			res.Unchanged++

		default:
			// Close the superseded row at this snapshot, then open the new
			// one. The old row stays readable for any score that named an
			// earlier snapshot.
			if _, err := tx.ExecContext(ctx,
				`UPDATE labels SET valid_to_snapshot = $2, last_updated = now() WHERE id = $1`,
				curID, snapshotID); err != nil {
				return res, fmt.Errorf("close label %d: %w", curID, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO labels
					(chain, address, entity, category, confidence, source, evidence, valid_from_snapshot)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				l.Chain, l.Address, l.Entity, l.Category, l.Confidence, l.Source, evidence, snapshotID,
			); err != nil {
				return res, fmt.Errorf("reopen label %s/%s: %w", l.Address, l.Source, err)
			}
			res.Updated++
		}
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit labels: %w", err)
	}
	return res, nil
}

// RetireAddresses closes, at this snapshot, the current labels of a source
// on the given addresses.
func (s *Store) RetireAddresses(ctx context.Context, snapshotID int64, source, chainID string, addrs []string) (int64, error) {
	if len(addrs) == 0 {
		return 0, nil
	}
	res, err := s.pg.ExecContext(ctx, `
		UPDATE labels SET valid_to_snapshot = $1, last_updated = now()
		WHERE source = $2 AND chain = $3 AND address = ANY($4) AND valid_to_snapshot IS NULL`,
		snapshotID, source, chainID, addrs)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DB is the store's database, for queries outside the label tables.
func (s *Store) DB() *sql.DB { return s.pg }

// Retire closes, at this snapshot, every current label of a source on a
// chain whose address is not in keep. It is for sources that publish their
// whole list each time, so an address that left the list stops being
// labelled rather than keeping a stale label forever.
func (s *Store) Retire(ctx context.Context, snapshotID int64, source, chainID string, keep map[string]bool) (int, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, address FROM labels
		WHERE source = $1 AND chain = $2 AND valid_to_snapshot IS NULL`, source, chainID)
	if err != nil {
		return 0, err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		var addr string
		if err := rows.Scan(&id, &addr); err != nil {
			rows.Close()
			return 0, err
		}
		if !keep[addr] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(gone) == 0 {
		return 0, nil
	}
	_, err = s.pg.ExecContext(ctx,
		`UPDATE labels SET valid_to_snapshot = $2, last_updated = now() WHERE id = ANY($1)`, gone, snapshotID)
	return len(gone), err
}

// ForAddress returns every label an address carries at a snapshot.
func (s *Store) ForAddress(ctx context.Context, snapshotID int64, chainID, address string) ([]Label, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, chain, address, entity, category, confidence, source, evidence
		FROM labels
		WHERE chain = $1 AND address = $2
		  AND valid_from_snapshot <= $3
		  AND (valid_to_snapshot IS NULL OR valid_to_snapshot > $3)
		ORDER BY id`,
		chainID, address, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("read labels for %s: %w", address, err)
	}
	defer rows.Close()
	return scanLabels(rows)
}

// ForAddresses batches the lookup traversal needs at every node.
//
// SPEC.md §7 makes a labelled address a terminal node, so this question is
// asked once per visited address. Doing it one row at a time would dominate
// traversal time on any real graph.
func (s *Store) ForAddresses(ctx context.Context, snapshotID int64, chainID string, addresses []string) (map[string][]Label, error) {
	out := make(map[string][]Label, len(addresses))
	if len(addresses) == 0 {
		return out, nil
	}

	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, chain, address, entity, category, confidence, source, evidence
		FROM labels
		WHERE chain = $1 AND address = ANY($2)
		  AND valid_from_snapshot <= $3
		  AND (valid_to_snapshot IS NULL OR valid_to_snapshot > $3)
		ORDER BY address, id`,
		chainID, addresses, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("batch read labels: %w", err)
	}
	defer rows.Close()

	all, err := scanLabels(rows)
	if err != nil {
		return nil, err
	}
	for _, l := range all {
		out[l.Address] = append(out[l.Address], l)
	}
	return out, nil
}

// ByCategories returns every label of the given categories on a chain at a
// snapshot.
func (s *Store) ByCategories(ctx context.Context, snapshotID int64, chainID string, categories []string) ([]Label, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, chain, address, entity, category, confidence, source, evidence
		FROM labels
		WHERE chain = $1 AND category = ANY($2)
		  AND valid_from_snapshot <= $3
		  AND (valid_to_snapshot IS NULL OR valid_to_snapshot > $3)
		ORDER BY address, id`,
		chainID, categories, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("read labels by category: %w", err)
	}
	defer rows.Close()
	return scanLabels(rows)
}

// BySources returns every label from the given sources on a chain at a
// snapshot.
func (s *Store) BySources(ctx context.Context, snapshotID int64, chainID string, sources []string) ([]Label, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT id, chain, address, entity, category, confidence, source, evidence
		FROM labels
		WHERE chain = $1 AND source = ANY($2)
		  AND valid_from_snapshot <= $3
		  AND (valid_to_snapshot IS NULL OR valid_to_snapshot > $3)
		ORDER BY address, id`,
		chainID, sources, snapshotID)
	if err != nil {
		return nil, fmt.Errorf("read labels by source: %w", err)
	}
	defer rows.Close()
	return scanLabels(rows)
}

func scanLabels(rows *sql.Rows) ([]Label, error) {
	var out []Label
	for rows.Next() {
		var l Label
		var evidence []byte
		if err := rows.Scan(&l.ID, &l.Chain, &l.Address, &l.Entity,
			&l.Category, &l.Confidence, &l.Source, &evidence); err != nil {
			return nil, err
		}
		if len(evidence) > 0 {
			_ = json.Unmarshal(evidence, &l.Evidence)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CountsBySource reports label counts per source at a snapshot, which SPEC.md
// §6 acceptance requires.
func (s *Store) CountsBySource(ctx context.Context, snapshotID int64) (map[string]int, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT source, count(*) FROM labels
		WHERE valid_from_snapshot <= $1
		  AND (valid_to_snapshot IS NULL OR valid_to_snapshot > $1)
		GROUP BY source ORDER BY source`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var source string
		var n int
		if err := rows.Scan(&source, &n); err != nil {
			return nil, err
		}
		out[source] = n
	}
	return out, rows.Err()
}

// RecordConflict stores a category disagreement. SPEC.md §6: never silently
// merge conflicting categories.
func (s *Store) RecordConflict(ctx context.Context, snapshotID int64, c *ConflictRecord) error {
	if c == nil {
		return nil
	}
	competing, err := json.Marshal(c.Competing)
	if err != nil {
		return err
	}
	_, err = s.pg.ExecContext(ctx, `
		INSERT INTO label_conflicts
			(chain, address, snapshot_id, resolved_category, resolved_source, competing)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (chain, address, snapshot_id) DO UPDATE SET
			resolved_category = EXCLUDED.resolved_category,
			resolved_source   = EXCLUDED.resolved_source,
			competing         = EXCLUDED.competing`,
		c.Chain, c.Address, snapshotID, c.ResolvedCategory, c.ResolvedSource, competing)
	if err != nil {
		return fmt.Errorf("record conflict for %s: %w", c.Address, err)
	}
	return nil
}

// UnreviewedConflicts returns conflicts awaiting human review.
func (s *Store) UnreviewedConflicts(ctx context.Context, limit int) ([]ConflictRecord, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT chain, address, resolved_category, resolved_source, competing
		FROM label_conflicts WHERE reviewed_at IS NULL
		ORDER BY detected_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConflictRecord
	for rows.Next() {
		var c ConflictRecord
		var competing []byte
		if err := rows.Scan(&c.Chain, &c.Address, &c.ResolvedCategory, &c.ResolvedSource, &competing); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(competing, &c.Competing)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CurrentAddresses is the set of addresses one source labels now.
func (s *Store) CurrentAddresses(ctx context.Context, source, chainID string) (map[string]bool, error) {
	rows, err := s.pg.QueryContext(ctx, `
		SELECT address FROM labels WHERE source = $1 AND chain = $2 AND valid_to_snapshot IS NULL`, source, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}
