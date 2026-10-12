package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing/projection"
)

// Leases is a [projection.LeaseStore] in PostgreSQL, for partitioned
// projections (see [projection.WithPartitions]). It uses the
// projection_leases and projection_members tables from [Schema].
//
// All lease timing uses the database's clock, now(), so clock differences
// between instances do not matter. Every instance heartbeats every third of
// the lease TTL, a few small writes per instance and projection; to keep
// those off a busy primary, use a LeaseStore on another system and keep
// [Checkpoints] here, since checkpoints must commit with the read model
// whatever holds the leases.
//
// Do not move a running projection between lease stores: their fences need
// not be comparable, and fences issued by the new store could be rejected
// by the checkpoints until they catch up. Moving is a rebuild under a new
// name.
type Leases struct {
	pool *pgxpool.Pool
}

// NewLeases returns a [Leases] that uses pool. The tables in [Schema] must
// exist. It can be the same pool as the [Checkpoints], or any other
// database the instances share.
func NewLeases(pool *pgxpool.Pool) *Leases {
	return &Leases{pool: pool}
}

// Init inserts a lease row per partition of name, without an owner.
func (s *Leases) Init(ctx context.Context, name string, partitions int) error {
	if partitions <= 0 {
		return fmt.Errorf("projection/postgres: partitions must be positive, got %d", partitions)
	}
	var existing int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM projection_leases WHERE projection = $1`, name).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 && existing != partitions {
		return fmt.Errorf("projection %s has %d partitions, want %d: %w", name, existing, partitions, projection.ErrPartitionsChanged)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO projection_leases (projection, partition)
		SELECT $1, g FROM generate_series(0, $2::int - 1) g
		ON CONFLICT DO NOTHING`, name, partitions)
	return err
}

// Heartbeat upserts member's last_seen, deletes members not seen for ten
// TTLs so the table does not grow forever, and counts those seen within
// one TTL.
func (s *Leases) Heartbeat(ctx context.Context, name, member string, ttl time.Duration) (int, error) {
	var live int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO projection_members (projection, member, last_seen) VALUES ($1, $2, now())
			ON CONFLICT (projection, member) DO UPDATE SET last_seen = now()`, name, member); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM projection_members
			WHERE projection = $1 AND last_seen < now() - make_interval(secs => $2::float8)`,
			name, (10 * ttl).Seconds()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM projection_members
			WHERE projection = $1 AND last_seen > now() - make_interval(secs => $2::float8)`,
			name, ttl.Seconds()).Scan(&live)
	})
	return live, err
}

// Claim picks up to n free or expired partitions with FOR UPDATE SKIP
// LOCKED, and gives each a new owner, expiry and fence + 1 in the same
// statement:
//
//	WITH c AS MATERIALIZED (
//		SELECT partition FROM projection_leases
//		WHERE projection = $1 AND (owner IS NULL OR lease_expires < now())
//		ORDER BY partition LIMIT $3
//		FOR UPDATE SKIP LOCKED
//	)
//	UPDATE projection_leases p SET owner = $2, lease_expires = now() + $4, fence = p.fence + 1
//	FROM c WHERE p.projection = $1 AND p.partition = c.partition
//	RETURNING p.partition, p.fence
//
// MATERIALIZED matters: as a plain subquery, the planner may run it once
// per row of a nested loop, and with SKIP LOCKED every run returns other
// rows, so LIMIT stops limiting.
func (s *Leases) Claim(ctx context.Context, name, member string, n int, ttl time.Duration) ([]projection.Lease, error) {
	if n <= 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		WITH c AS MATERIALIZED (
			SELECT partition FROM projection_leases
			WHERE projection = $1 AND (owner IS NULL OR lease_expires < now())
			ORDER BY partition
			LIMIT $3
			FOR UPDATE SKIP LOCKED
		)
		UPDATE projection_leases p
		SET owner = $2,
		    lease_expires = now() + make_interval(secs => $4::float8),
		    fence = p.fence + 1
		FROM c
		WHERE p.projection = $1 AND p.partition = c.partition
		RETURNING p.partition, p.fence`,
		name, member, n, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (projection.Lease, error) {
		l := projection.Lease{Projection: name, Member: member}
		err := row.Scan(&l.Partition, &l.Fence)
		return l, err
	})
}

// Renew extends every lease owned by member, expired or not. That is safe:
// an instance that claimed an expired lease has changed its owner and
// fence.
func (s *Leases) Renew(ctx context.Context, name, member string, ttl time.Duration) (map[int]int64, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE projection_leases
		SET lease_expires = now() + make_interval(secs => $3::float8)
		WHERE projection = $1 AND owner = $2
		RETURNING partition, fence`,
		name, member, ttl.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	held := make(map[int]int64)
	for rows.Next() {
		var (
			p     int
			fence int64
		)
		if err := rows.Scan(&p, &fence); err != nil {
			return nil, err
		}
		held[p] = fence
	}
	return held, rows.Err()
}

// Release clears the owner of l's partition if l's member and fence still
// hold it.
func (s *Leases) Release(ctx context.Context, l projection.Lease) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE projection_leases SET owner = NULL, lease_expires = NULL
		WHERE projection = $1 AND partition = $2 AND owner = $3 AND fence = $4`,
		l.Projection, l.Partition, l.Member, l.Fence)
	return err
}

// Leave clears every lease member holds and deletes it from
// projection_members.
func (s *Leases) Leave(ctx context.Context, name, member string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE projection_leases SET owner = NULL, lease_expires = NULL
			WHERE projection = $1 AND owner = $2`, name, member); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM projection_members WHERE projection = $1 AND member = $2`, name, member)
		return err
	})
}

var _ projection.LeaseStore = (*Leases)(nil)
