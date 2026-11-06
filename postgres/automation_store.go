package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing/automation"
)

var _ automation.Store = (*AutomationStore)(nil)

// AutomationStore is the Postgres [automation.Store]: it keeps which
// instance holds each segment of an automation, and the segment's pauses,
// in the automation_segments table that [Schema] creates. Create it with
// [NewAutomationStore] and pass it as [automation.Definition.Store].
//
// Every instance that uses the same database shares the leases, so a
// segment is worked by one instance at a time. Time is the database's
// clock, so the instances' clocks need not agree.
type AutomationStore struct {
	db *pgxpool.Pool
}

// NewAutomationStore returns an AutomationStore on db, which must hold the
// tables [Schema] creates.
func NewAutomationStore(db *pgxpool.Pool) *AutomationStore {
	return &AutomationStore{db: db}
}

// Acquire implements [automation.Store]. It runs in one transaction that
// records the candidates and locks their rows, in key order so that two
// instances acquiring at once wait for each other instead of deadlocking;
// it then reads them in fairness order and grants the first eligible ones.
func (s *AutomationStore) Acquire(ctx context.Context, name, owner string, candidates []automation.Segment, max int, ttl time.Duration) (automation.AcquireResult, error) {
	var res automation.AcquireResult
	keys, groups := uniqueCandidates(candidates)
	if len(keys) == 0 {
		return res, nil
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		// DO UPDATE, unlike DO NOTHING, locks an existing row. A candidate
		// without a group keeps the group it was recorded with.
		if _, err := tx.Exec(ctx, `
			INSERT INTO automation_segments AS s (name, segment_key, group_key, first_seen)
			SELECT $1, c.key, c.grp, now()
			FROM unnest($2::text[], $3::text[]) AS c(key, grp)
			ORDER BY c.key COLLATE "C"
			ON CONFLICT (name, segment_key) DO UPDATE
			SET group_key = CASE WHEN EXCLUDED.group_key <> '' THEN EXCLUDED.group_key ELSE s.group_key END`,
			name, keys, groups,
		); err != nil {
			return fmt.Errorf("record candidates: %w", err)
		}

		// The fairness order of automation.Store: round-robin across groups
		// by each group's last turn (the latest last_served of its keys,
		// else its earliest first_seen), and within a group by how long each
		// key has waited. Ordering by the rank within the group first makes
		// a multi-grant interleave groups. Keys compare byte-wise, as in Go.
		rows, err := tx.Query(ctx, `
			SELECT segment_key,
			       owner <> '' AND coalesce(lease_until > now(), false) AS held,
			       coalesce(paused_until > now(), false) AS paused
			FROM (
				SELECT segment_key, group_key, owner, lease_until, paused_until,
				       row_number() OVER (PARTITION BY group_key
				                          ORDER BY coalesce(last_served, first_seen), segment_key COLLATE "C") AS rank,
				       coalesce(max(last_served) OVER (PARTITION BY group_key),
				                min(first_seen) OVER (PARTITION BY group_key)) AS turn
				FROM automation_segments
				WHERE name = $1 AND segment_key = ANY($2)
			) c
			ORDER BY rank, turn, group_key COLLATE "C", segment_key COLLATE "C"`,
			name, keys,
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
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read candidates: %w", err)
		}
		if len(grant) == 0 {
			return nil
		}

		rows, err = tx.Query(ctx, `
			UPDATE automation_segments
			SET owner = $3, epoch = epoch + 1, lease_until = now() + $4 * interval '1 millisecond'
			WHERE name = $1 AND segment_key = ANY($2)
			RETURNING segment_key, epoch`,
			name, grant, owner, ttl.Milliseconds(),
		)
		if err != nil {
			return fmt.Errorf("grant: %w", err)
		}
		epochs := make(map[string]int64, len(grant))
		for rows.Next() {
			var (
				key   string
				epoch int64
			)
			if err := rows.Scan(&key, &epoch); err != nil {
				rows.Close()
				return fmt.Errorf("grant: %w", err)
			}
			epochs[key] = epoch
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("grant: %w", err)
		}
		for _, key := range grant {
			epoch, ok := epochs[key]
			if !ok {
				return fmt.Errorf("grant %q: row vanished", key)
			}
			res.Granted = append(res.Granted, automation.SegmentLease{Name: name, Key: key, Owner: owner, Epoch: uint64(epoch)})
		}
		return nil
	})
	if err != nil {
		return automation.AcquireResult{}, fmt.Errorf("postgres: acquire %q: %w", name, err)
	}
	return res, nil
}

// Renew implements [automation.Store] in one statement. A lease is renewed
// only while its owner and epoch are current and it has not expired.
func (s *AutomationStore) Renew(ctx context.Context, held []automation.SegmentLease, ttl time.Duration) ([]automation.SegmentLease, error) {
	if len(held) == 0 {
		return nil, nil
	}
	names, keys, owners, epochs := leaseColumns(held)
	rows, err := s.db.Query(ctx, `
		UPDATE automation_segments s
		SET lease_until = now() + $5 * interval '1 millisecond'
		FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[]) AS h(name, key, owner, epoch)
		WHERE s.name = h.name AND s.segment_key = h.key AND s.owner = h.owner AND s.epoch = h.epoch
		  AND s.lease_until > now()
		RETURNING s.name, s.segment_key, s.owner, s.epoch`,
		names, keys, owners, epochs, ttl.Milliseconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: renew: %w", err)
	}
	lost, err := unmatched(rows, held)
	if err != nil {
		return nil, fmt.Errorf("postgres: renew: %w", err)
	}
	return lost, nil
}

// Release implements [automation.Store] in one statement. Each release is
// fenced on owner and epoch; a lost one changes nothing.
func (s *AutomationStore) Release(ctx context.Context, rel []automation.ReleaseRequest) ([]automation.SegmentLease, error) {
	if len(rel) == 0 {
		return nil, nil
	}
	leases := make([]automation.SegmentLease, len(rel))
	deferMS := make([]int64, len(rel))
	served := make([]bool, len(rel))
	for i, rr := range rel {
		leases[i] = rr.Lease
		deferMS[i] = rr.DeferFor.Milliseconds()
		served[i] = rr.Served
	}
	names, keys, owners, epochs := leaseColumns(leases)
	// GREATEST ignores NULL, so a first pause needs no special case.
	rows, err := s.db.Query(ctx, `
		UPDATE automation_segments s
		SET owner = '', lease_until = NULL,
		    paused_until = CASE WHEN h.defer_ms > 0
		                        THEN greatest(s.paused_until, now() + h.defer_ms * interval '1 millisecond')
		                        ELSE s.paused_until END,
		    last_served = CASE WHEN h.served THEN now() ELSE s.last_served END
		FROM unnest($1::text[], $2::text[], $3::text[], $4::bigint[], $5::bigint[], $6::bool[])
		     AS h(name, key, owner, epoch, defer_ms, served)
		WHERE s.name = h.name AND s.segment_key = h.key AND s.owner = h.owner AND s.epoch = h.epoch
		RETURNING s.name, s.segment_key, h.owner, h.epoch`,
		names, keys, owners, epochs, deferMS, served,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: release: %w", err)
	}
	lost, err := unmatched(rows, leases)
	if err != nil {
		return nil, fmt.Errorf("postgres: release: %w", err)
	}
	return lost, nil
}

// Defer implements [automation.Store]. It records key if it is new, and
// keeps a longer pause that is already set.
func (s *AutomationStore) Defer(ctx context.Context, name, key string, d time.Duration) error {
	if _, err := s.db.Exec(ctx, `
		INSERT INTO automation_segments AS s (name, segment_key, paused_until)
		VALUES ($1, $2, now() + $3 * interval '1 millisecond')
		ON CONFLICT (name, segment_key) DO UPDATE
		SET paused_until = greatest(s.paused_until, EXCLUDED.paused_until)`,
		name, key, d.Milliseconds(),
	); err != nil {
		return fmt.Errorf("postgres: defer %q segment %q: %w", name, key, err)
	}
	return nil
}

// DeferredFor implements [automation.Store].
func (s *AutomationStore) DeferredFor(ctx context.Context, name, key string) (time.Duration, error) {
	var ms int64
	err := s.db.QueryRow(ctx, `
		SELECT coalesce(greatest(ceil(EXTRACT(EPOCH FROM (paused_until - now())) * 1000), 0), 0)::bigint
		FROM automation_segments
		WHERE name = $1 AND segment_key = $2`,
		name, key,
	).Scan(&ms)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: deferred for %q segment %q: %w", name, key, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// uniqueCandidates splits segs into keys and groups, dropping repeated
// keys and keeping the first, as [automation.LocalStore] does.
func uniqueCandidates(segs []automation.Segment) (keys, groups []string) {
	seen := make(map[string]bool, len(segs))
	for _, s := range segs {
		if seen[s.Key] {
			continue
		}
		seen[s.Key] = true
		keys = append(keys, s.Key)
		groups = append(groups, s.Group)
	}
	return keys, groups
}

func leaseColumns(ls []automation.SegmentLease) (names, keys, owners []string, epochs []int64) {
	for _, l := range ls {
		names = append(names, l.Name)
		keys = append(keys, l.Key)
		owners = append(owners, l.Owner)
		epochs = append(epochs, int64(l.Epoch))
	}
	return names, keys, owners, epochs
}

// unmatched reads the (name, key, owner, epoch) rows a fenced update
// returned and gives back the leases in want that are not among them.
func unmatched(rows pgx.Rows, want []automation.SegmentLease) ([]automation.SegmentLease, error) {
	defer rows.Close()
	done := map[automation.SegmentLease]bool{}
	for rows.Next() {
		var (
			l     automation.SegmentLease
			epoch int64
		)
		if err := rows.Scan(&l.Name, &l.Key, &l.Owner, &epoch); err != nil {
			return nil, err
		}
		l.Epoch = uint64(epoch)
		done[l] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var lost []automation.SegmentLease
	for _, l := range want {
		if !done[l] {
			lost = append(lost, l)
		}
	}
	return lost, nil
}
