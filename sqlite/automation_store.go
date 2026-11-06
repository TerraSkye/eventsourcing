package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing/automation"
)

var _ automation.Store = (*AutomationStore)(nil)

// AutomationStore is the SQLite [automation.Store]: it keeps which instance
// holds each segment of an automation, and the segment's pauses, in the
// automation_segments table that [Schema] creates. Create it with
// [NewAutomationStore] and pass it as [automation.Definition.Store].
//
// Every process that opens the same database file shares the leases, so a
// segment is worked by one process at a time. Time is SQLite's clock, in
// milliseconds.
type AutomationStore struct {
	db *sql.DB
}

// NewAutomationStore returns an AutomationStore on db, which must hold the
// tables [Schema] creates. Open db with [Open].
func NewAutomationStore(db *sql.DB) *AutomationStore {
	return &AutomationStore{db: db}
}

// candidate is one segment as Acquire passes it to SQLite through json_each.
type candidate struct {
	Key   string `json:"k"`
	Group string `json:"g"`
}

// Acquire implements [automation.Store]. It runs in one transaction, which
// holds SQLite's write lock: it records the candidates, reads them in
// fairness order, and grants the first eligible ones.
func (s *AutomationStore) Acquire(ctx context.Context, name, owner string, candidates []automation.Segment, max int, ttl time.Duration) (automation.AcquireResult, error) {
	var res automation.AcquireResult
	cands := uniqueCandidates(candidates)
	if len(cands) == 0 {
		return res, nil
	}
	js, err := json.Marshal(cands)
	if err != nil {
		return res, fmt.Errorf("sqlite: acquire %q: %w", name, err)
	}
	err = s.runTx(ctx, fmt.Sprintf("acquire %q", name), func(tx *sql.Tx) error {
		// A candidate without a group keeps the group it was recorded with.
		// WHERE true tells SQLite's parser that ON CONFLICT belongs to the
		// INSERT, not to a join in the SELECT.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO automation_segments (name, segment_key, group_key, first_seen)
			SELECT ?1, json_extract(value, '$.k'), json_extract(value, '$.g'), `+nowMS+`
			FROM json_each(?2) WHERE true
			ON CONFLICT (name, segment_key) DO UPDATE SET group_key = excluded.group_key
			WHERE excluded.group_key <> ''`,
			name, string(js),
		); err != nil {
			return fmt.Errorf("record candidates: %w", err)
		}

		// The fairness order of automation.Store: round-robin across groups
		// by each group's last turn (the latest last_served of its keys,
		// else its earliest first_seen), and within a group by how long each
		// key has waited. Ordering by the rank within the group first makes
		// a multi-grant interleave groups.
		rows, err := tx.QueryContext(ctx, `
			SELECT segment_key,
			       owner <> '' AND lease_until > now_ms AS held,
			       paused_until > now_ms AS paused
			FROM (
				SELECT segment_key, group_key, owner, lease_until, paused_until, `+nowMS+` AS now_ms,
				       row_number() OVER (PARTITION BY group_key
				                          ORDER BY coalesce(last_served, first_seen), segment_key) AS rank,
				       coalesce(max(last_served) OVER (PARTITION BY group_key),
				                min(first_seen) OVER (PARTITION BY group_key)) AS turn
				FROM automation_segments
				WHERE name = ?1 AND segment_key IN (SELECT json_extract(value, '$.k') FROM json_each(?2))
			)
			ORDER BY rank, turn, group_key, segment_key`,
			name, string(js),
		)
		if err != nil {
			return fmt.Errorf("read candidates: %w", err)
		}
		var grant []string
		for rows.Next() {
			var (
				key          string
				held, paused bool
			)
			if err := rows.Scan(&key, &held, &paused); err != nil {
				rows.Close()
				return fmt.Errorf("read candidates: %w", err)
			}
			switch {
			case paused:
			case held:
				res.Active++
			case len(grant) < max:
				res.Active++
				grant = append(grant, key)
			default:
				res.Active++
				res.Eligible++
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("read candidates: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read candidates: %w", err)
		}

		for _, key := range grant {
			var epoch int64
			if err := tx.QueryRowContext(ctx, `
				UPDATE automation_segments
				SET owner = ?3, epoch = epoch + 1, lease_until = `+nowMS+` + ?4
				WHERE name = ?1 AND segment_key = ?2
				RETURNING epoch`,
				name, key, owner, ttl.Milliseconds(),
			).Scan(&epoch); err != nil {
				return fmt.Errorf("grant %q: %w", key, err)
			}
			res.Granted = append(res.Granted, automation.SegmentLease{Name: name, Key: key, Owner: owner, Epoch: uint64(epoch)})
		}
		return nil
	})
	if err != nil {
		return automation.AcquireResult{}, err
	}
	return res, nil
}

// Renew implements [automation.Store]. A lease is renewed only while its
// owner and epoch are current and it has not expired.
func (s *AutomationStore) Renew(ctx context.Context, held []automation.SegmentLease, ttl time.Duration) ([]automation.SegmentLease, error) {
	var lost []automation.SegmentLease
	err := s.runTx(ctx, "renew", func(tx *sql.Tx) error {
		for _, l := range held {
			res, err := tx.ExecContext(ctx, `
				UPDATE automation_segments SET lease_until = `+nowMS+` + ?5
				WHERE name = ?1 AND segment_key = ?2 AND owner = ?3 AND epoch = ?4
				  AND lease_until > `+nowMS,
				l.Name, l.Key, l.Owner, int64(l.Epoch), ttl.Milliseconds(),
			)
			if err != nil {
				return fmt.Errorf("%q segment %q: %w", l.Name, l.Key, err)
			}
			if ok, err := changed(res); err != nil {
				return err
			} else if !ok {
				lost = append(lost, l)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lost, nil
}

// Release implements [automation.Store]. Each release is fenced on owner
// and epoch; a lost one changes nothing.
func (s *AutomationStore) Release(ctx context.Context, rel []automation.ReleaseRequest) ([]automation.SegmentLease, error) {
	var lost []automation.SegmentLease
	err := s.runTx(ctx, "release", func(tx *sql.Tx) error {
		for _, rr := range rel {
			l := rr.Lease
			res, err := tx.ExecContext(ctx, `
				UPDATE automation_segments
				SET owner = '', lease_until = 0,
				    paused_until = CASE WHEN ?5 > 0 THEN max(paused_until, `+nowMS+` + ?5) ELSE paused_until END,
				    last_served = CASE WHEN ?6 THEN `+nowMS+` ELSE last_served END
				WHERE name = ?1 AND segment_key = ?2 AND owner = ?3 AND epoch = ?4`,
				l.Name, l.Key, l.Owner, int64(l.Epoch), rr.DeferFor.Milliseconds(), rr.Served,
			)
			if err != nil {
				return fmt.Errorf("%q segment %q: %w", l.Name, l.Key, err)
			}
			if ok, err := changed(res); err != nil {
				return err
			} else if !ok {
				lost = append(lost, l)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return lost, nil
}

// Defer implements [automation.Store]. It records key if it is new, and
// keeps a longer pause that is already set.
func (s *AutomationStore) Defer(ctx context.Context, name, key string, d time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO automation_segments (name, segment_key, first_seen, paused_until)
		VALUES (?1, ?2, `+nowMS+`, `+nowMS+` + ?3)
		ON CONFLICT (name, segment_key) DO UPDATE
		SET paused_until = max(paused_until, excluded.paused_until)`,
		name, key, d.Milliseconds(),
	); err != nil {
		return fmt.Errorf("sqlite: defer %q segment %q: %w", name, key, err)
	}
	return nil
}

// DeferredFor implements [automation.Store].
func (s *AutomationStore) DeferredFor(ctx context.Context, name, key string) (time.Duration, error) {
	var ms int64
	err := s.db.QueryRowContext(ctx, `
		SELECT max(paused_until - `+nowMS+`, 0) FROM automation_segments
		WHERE name = ? AND segment_key = ?`,
		name, key,
	).Scan(&ms)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite: deferred for %q segment %q: %w", name, key, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// runTx runs fn in a transaction; op names the call in errors.
func (s *AutomationStore) runTx(ctx context.Context, op string, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: %s: begin: %w", op, err)
	}
	// Rollback after Commit is a no-op.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return fmt.Errorf("sqlite: %s: %w", op, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: %s: commit: %w", op, err)
	}
	return nil
}

// changed reports whether a fenced update changed a row.
func changed(res sql.Result) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// uniqueCandidates drops repeated keys, keeping the first, as
// [automation.LocalStore] does.
func uniqueCandidates(segs []automation.Segment) []candidate {
	seen := make(map[string]bool, len(segs))
	out := make([]candidate, 0, len(segs))
	for _, s := range segs {
		if seen[s.Key] {
			continue
		}
		seen[s.Key] = true
		out = append(out, candidate{Key: s.Key, Group: s.Group})
	}
	return out
}
