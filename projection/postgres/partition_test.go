//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventstore/memory"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/postgres"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

type ItemAdded struct {
	OrderID string
	Amount  int
}

func (e *ItemAdded) AggregateID() string { return e.OrderID }
func (e *ItemAdded) EventType() string   { return "ItemAdded" }

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	eventsourcing.RegisterEvent(&ItemAdded{})
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("testdb"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
	)
	if err != nil {
		log.Fatalf("start postgres container: %v", err)
	}
	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		log.Fatalf("connection string: %v", err)
	}
	pool, err = pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	for i := range 20 {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if i == 19 {
			log.Fatalf("ping postgres: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := pool.Exec(ctx, postgres.Schema); err != nil {
		log.Fatalf("apply schema: %v", err)
	}

	code := m.Run()
	pool.Close()
	_ = pgc.Terminate(ctx)
	os.Exit(code)
}

func uniqueName(t *testing.T) string {
	return fmt.Sprintf("%s-%s", t.Name(), uuid.NewString()[:8])
}

// --- Leases ---

func TestLeases_ClaimsAreExclusiveAndFencesIncrease(t *testing.T) {
	ctx := t.Context()
	leases := postgres.NewLeases(pool)
	name := uniqueName(t)
	ttl := time.Minute

	if err := leases.Init(ctx, name, 8); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := leases.Init(ctx, name, 8); err != nil {
		t.Fatalf("Init again with the same count: %v", err)
	}
	if err := leases.Init(ctx, name, 4); !errors.Is(err, projection.ErrPartitionsChanged) {
		t.Fatalf("Init with another count = %v, want ErrPartitionsChanged", err)
	}

	a, err := leases.Claim(ctx, name, "a", 8, ttl)
	if err != nil || len(a) != 8 {
		t.Fatalf("a claims 8 = %d leases, %v; want 8", len(a), err)
	}
	if b, err := leases.Claim(ctx, name, "b", 8, ttl); err != nil || len(b) != 0 {
		t.Fatalf("b claims while a holds all = %d leases, %v; want 0", len(b), err)
	}

	if err := leases.Release(ctx, a[3]); err != nil {
		t.Fatalf("Release: %v", err)
	}
	b, err := leases.Claim(ctx, name, "b", 8, ttl)
	if err != nil || len(b) != 1 || b[0].Partition != a[3].Partition {
		t.Fatalf("b claims after release = %+v, %v; want partition %d", b, err, a[3].Partition)
	}
	if b[0].Fence <= a[3].Fence {
		t.Errorf("fence after reclaim = %d, want > %d", b[0].Fence, a[3].Fence)
	}

	// A release with the old fence must not free b's lease.
	if err := leases.Release(ctx, a[3]); err != nil {
		t.Fatalf("stale Release: %v", err)
	}
	held, err := leases.Renew(ctx, name, "b", ttl)
	if err != nil || held[b[0].Partition] != b[0].Fence {
		t.Fatalf("b renews = %v, %v; want partition %d at fence %d", held, err, b[0].Partition, b[0].Fence)
	}
	held, err = leases.Renew(ctx, name, "a", ttl)
	if err != nil || len(held) != 7 {
		t.Fatalf("a renews = %d leases, %v; want 7", len(held), err)
	}

	if err := leases.Leave(ctx, name, "a"); err != nil {
		t.Fatalf("Leave: %v", err)
	}
	if got, err := leases.Claim(ctx, name, "b", 8, ttl); err != nil || len(got) != 7 {
		t.Fatalf("b claims after a left = %d leases, %v; want 7", len(got), err)
	}
}

func TestLeases_ExpiredLeaseIsClaimedWithHigherFence(t *testing.T) {
	ctx := t.Context()
	leases := postgres.NewLeases(pool)
	name := uniqueName(t)
	if err := leases.Init(ctx, name, 1); err != nil {
		t.Fatal(err)
	}
	a, err := leases.Claim(ctx, name, "a", 1, 500*time.Millisecond)
	if err != nil || len(a) != 1 {
		t.Fatalf("claim: %v %v", a, err)
	}
	time.Sleep(time.Second)
	b, err := leases.Claim(ctx, name, "b", 1, time.Minute)
	if err != nil || len(b) != 1 || b[0].Fence <= a[0].Fence {
		t.Fatalf("claim of expired lease = %+v, %v; want fence > %d", b, err, a[0].Fence)
	}
	if held, _ := leases.Renew(ctx, name, "a", time.Minute); len(held) != 0 {
		t.Errorf("a still renews %v after b took over", held)
	}
}

func TestLeases_HeartbeatCountsLiveMembers(t *testing.T) {
	ctx := t.Context()
	leases := postgres.NewLeases(pool)
	name := uniqueName(t)
	for i, m := range []string{"a", "b", "c"} {
		live, err := leases.Heartbeat(ctx, name, m, time.Minute)
		if err != nil || live != i+1 {
			t.Fatalf("heartbeat %s = %d, %v; want %d", m, live, err, i+1)
		}
	}
	if err := leases.Leave(ctx, name, "b"); err != nil {
		t.Fatal(err)
	}
	if live, err := leases.Heartbeat(ctx, name, "a", time.Minute); err != nil || live != 2 {
		t.Fatalf("heartbeat after leave = %d, %v; want 2", live, err)
	}
}

// --- Fenced checkpoints ---

func TestCheckpoints_StaleOwnerIsFencedOut(t *testing.T) {
	ctx := t.Context()
	cps := postgres.NewCheckpoints(pool)
	name := uniqueName(t)
	if err := cps.InitPartitions(ctx, name, 2); err != nil {
		t.Fatal(err)
	}
	old := projection.Lease{Projection: name, Partition: 1, Member: "a", Fence: 1}
	if _, err := cps.Acquire(ctx, old); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := cps.CommitPartition(ctx, old, projection.Checkpoint{Position: 5}, noop); err != nil {
		t.Fatalf("CommitPartition: %v", err)
	}

	cur := projection.Lease{Projection: name, Partition: 1, Member: "b", Fence: 2}
	cp, err := cps.Acquire(ctx, cur)
	if err != nil || cp.Position != 5 {
		t.Fatalf("new owner Acquire = %+v, %v; want position 5", cp, err)
	}

	ran := false
	err = cps.CommitPartition(ctx, old, projection.Checkpoint{Position: 9}, func(context.Context) error {
		ran = true
		return nil
	})
	if !errors.Is(err, projection.ErrLeaseLost) {
		t.Fatalf("stale CommitPartition = %v, want ErrLeaseLost", err)
	}
	if ran {
		t.Error("stale CommitPartition ran fn; the fence must be checked first")
	}
	if _, err := cps.Acquire(ctx, old); !errors.Is(err, projection.ErrLeaseLost) {
		t.Fatalf("stale Acquire = %v, want ErrLeaseLost", err)
	}
}

func TestCheckpoints_CommitRollsBackWithFn(t *testing.T) {
	ctx := t.Context()
	cps := postgres.NewCheckpoints(pool)
	name := uniqueName(t)
	l := projection.Lease{Projection: name, Partition: 0, Member: "a", Fence: 1}
	if _, err := cps.Acquire(ctx, l); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := cps.CommitPartition(ctx, l, projection.Checkpoint{Position: 3}, func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("CommitPartition = %v, want boom", err)
	}
	cp, err := cps.Acquire(ctx, l)
	if err != nil || cp.Position != 0 {
		t.Fatalf("checkpoint after failed commit = %+v, %v; want position 0", cp, err)
	}
}

func TestCheckpoints_AcquireWaitsForInFlightBatch(t *testing.T) {
	ctx := t.Context()
	cps := postgres.NewCheckpoints(pool)
	name := uniqueName(t)
	old := projection.Lease{Projection: name, Partition: 0, Member: "a", Fence: 1}
	if _, err := cps.Acquire(ctx, old); err != nil {
		t.Fatal(err)
	}

	started, release := make(chan struct{}), make(chan struct{})
	committed := make(chan error, 1)
	go func() {
		committed <- cps.CommitPartition(ctx, old, projection.Checkpoint{Position: 7}, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	acquired := make(chan projection.Checkpoint, 1)
	go func() {
		cp, err := cps.Acquire(ctx, projection.Lease{Projection: name, Partition: 0, Member: "b", Fence: 2})
		if err != nil {
			t.Errorf("new owner Acquire: %v", err)
		}
		acquired <- cp
	}()

	select {
	case cp := <-acquired:
		t.Fatalf("Acquire returned %+v while the old owner's batch was in flight", cp)
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatalf("in-flight CommitPartition: %v", err)
	}
	if cp := <-acquired; cp.Position != 7 {
		t.Fatalf("Acquire after the batch = position %d, want 7", cp.Position)
	}
}

func noop(context.Context) error { return nil }

// --- Partitioned runner, end to end ---

const totalsSchema = `
CREATE TABLE IF NOT EXISTS %[1]s (
    order_id TEXT PRIMARY KEY,
    items    INT  NOT NULL,
    total    INT  NOT NULL,
    seen     BIGINT[] NOT NULL
)`

// totals is a read model with one row per order. It records every
// GlobalVersion it applied, so a duplicate or missing event shows up
// exactly.
type totals struct {
	table string
	fail  func(env uint64) error
}

func (p *totals) OnItemAdded(ctx context.Context, e *ItemAdded) error {
	gv := eventsourcing.GlobalVersionFromContext(ctx)
	if p.fail != nil {
		if err := p.fail(gv); err != nil {
			return err
		}
	}
	_, err := postgres.Tx(ctx).Exec(ctx, fmt.Sprintf(`
		INSERT INTO %s (order_id, items, total, seen) VALUES ($1, 1, $2, ARRAY[$3::bigint])
		ON CONFLICT (order_id) DO UPDATE
		SET items = %[1]s.items + 1, total = %[1]s.total + $2, seen = %[1]s.seen || $3::bigint`, p.table),
		e.OrderID, e.Amount, int64(gv))
	return err
}

func (p *totals) handlers() *eventsourcing.EventGroupProcessor {
	return eventsourcing.NewEventGroupProcessor(eventsourcing.OnEvent(p.OnItemAdded))
}

type orderWant struct {
	items, total int
	seen         map[uint64]bool
}

// appendOrders appends n ItemAdded events over orders streams from
// writers concurrent goroutines.
func appendOrders(t *testing.T, store *memory.MemoryStore, writers, n, orders int) {
	t.Helper()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Go(func() {
			for i := range n / writers {
				id := fmt.Sprintf("order-%d", (w*7919+i)%orders)
				_, err := store.Save(context.Background(), []eventsourcing.Envelope{{
					EventID:  uuid.New(),
					StreamID: id,
					Event:    &ItemAdded{OrderID: id, Amount: 1 + rand.IntN(9)},
				}}, eventsourcing.Any{})
				if err != nil {
					t.Errorf("save: %v", err)
					return
				}
				if i%25 == 0 {
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
	wg.Wait()
}

func expected(t *testing.T, store *memory.MemoryStore) map[string]*orderWant {
	t.Helper()
	want := map[string]*orderWant{}
	var after uint64
	for {
		res, err := store.ReadAll(t.Context(), after, eventsourcing.ReadOptions{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, env := range res.Events {
			e := env.Event.(*ItemAdded)
			w := want[e.OrderID]
			if w == nil {
				w = &orderWant{seen: map[uint64]bool{}}
				want[e.OrderID] = w
			}
			w.items++
			w.total += e.Amount
			w.seen[env.GlobalVersion] = true
		}
		after = res.Next
		if res.AtEnd {
			return want
		}
	}
}

func checkTotals(t *testing.T, table string, want map[string]*orderWant) {
	t.Helper()
	rows, err := pool.Query(t.Context(), fmt.Sprintf(`SELECT order_id, items, total, seen FROM %s`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := 0
	for rows.Next() {
		var (
			id           string
			items, total int
			seen         []int64
		)
		if err := rows.Scan(&id, &items, &total, &seen); err != nil {
			t.Fatal(err)
		}
		got++
		w := want[id]
		if w == nil {
			t.Errorf("order %s in read model but never written", id)
			continue
		}
		if items != w.items || total != w.total || len(seen) != len(w.seen) {
			t.Errorf("order %s: items %d total %d events %d; want %d %d %d", id, items, total, len(seen), w.items, w.total, len(w.seen))
		}
		dup := map[int64]bool{}
		for i, gv := range seen {
			if dup[gv] {
				t.Errorf("order %s: event %d applied twice", id, gv)
			}
			dup[gv] = true
			if i > 0 && seen[i-1] > gv {
				t.Errorf("order %s: event %d applied after %d, out of log order", id, gv, seen[i-1])
			}
		}
	}
	if got != len(want) {
		t.Errorf("read model has %d orders, want %d", got, len(want))
	}
}

type instance struct {
	runner *projection.Runner
	cancel context.CancelFunc
	done   chan error
}

func startInstance(t *testing.T, name, member string, p *totals, store *memory.MemoryStore, partitions int) *instance {
	t.Helper()
	r := projection.NewRunner(name, p.handlers(), store,
		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
		projection.WithPartitions(partitions, projection.ByStreamID, postgres.NewLeases(pool)),
		projection.WithMember(member),
		projection.WithLeaseTTL(2*time.Second),
		projection.WithPollInterval(50*time.Millisecond),
		projection.WithBatchSize(50),
		projection.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	)
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{runner: r, cancel: cancel, done: make(chan error, 1)}
	go func() { inst.done <- r.Run(ctx) }()
	t.Cleanup(inst.stop)
	return inst
}

func (i *instance) stop() {
	i.cancel()
	<-i.done
	i.done <- nil // let a second stop return at once
}

func newTotals(t *testing.T) *totals {
	t.Helper()
	table := fmt.Sprintf("totals_%s", uuid.NewString()[:8])
	if _, err := pool.Exec(t.Context(), fmt.Sprintf(totalsSchema, table)); err != nil {
		t.Fatal(err)
	}
	return &totals{table: table}
}

func waitLive(t *testing.T, r *projection.Runner, head uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := r.WaitUntil(ctx, head); err != nil {
		t.Fatalf("WaitUntil(%d): %v", head, err)
	}
	for {
		live, err := r.Live(ctx)
		if err != nil {
			t.Fatalf("Live: %v", err)
		}
		if live {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("projection reached %d but never reported live", head)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestPartitionedRunner_ExactlyOnceWhileInstancesComeAndGo(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)
	const partitions = 8

	appendOrders(t, store, 4, 400, 30) // history before anything runs

	a := startInstance(t, name, "a", p, store, partitions)
	b := startInstance(t, name, "b", p, store, partitions)

	var writing sync.WaitGroup
	writing.Go(func() { appendOrders(t, store, 8, 1600, 60) })

	time.Sleep(700 * time.Millisecond)
	a.stop() // hands its partitions over mid-stream
	c := startInstance(t, name, "c", p, store, partitions)
	time.Sleep(700 * time.Millisecond)
	_ = startInstance(t, name, "d", p, store, partitions)

	writing.Wait()
	head, err := store.Head(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitLive(t, c.runner, head)
	checkTotals(t, p.table, expected(t, store))

	var behind int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM projection_checkpoints WHERE name = $1 AND position < $2`,
		name, int64(head)).Scan(&behind); err != nil {
		t.Fatal(err)
	}
	if behind != 0 {
		t.Errorf("%d partitions behind the head after catching up, want 0", behind)
	}

	st := b.runner.Status()
	if st.Partitions != partitions || len(st.Owned) == 0 {
		t.Errorf("b's status = %+v; want %d partitions and a share of them", st, partitions)
	}
}

func TestPartitionedRunner_ResumesFromCheckpointAfterRestart(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)

	appendOrders(t, store, 2, 300, 20)
	a := startInstance(t, name, "a", p, store, 4)
	head, _ := store.Head(t.Context())
	waitLive(t, a.runner, head)
	a.stop()

	appendOrders(t, store, 2, 300, 20)
	a2 := startInstance(t, name, "a", p, store, 4)
	head, _ = store.Head(t.Context())
	waitLive(t, a2.runner, head)
	checkTotals(t, p.table, expected(t, store))
}

func TestPartitionedRunner_FailingHandlerStallsThenRecovers(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)

	appendOrders(t, store, 1, 100, 10)
	var failures atomic.Int32
	p.fail = func(gv uint64) error {
		if gv == 50 && failures.Add(1) <= 2 {
			return errors.New("transient")
		}
		return nil
	}

	a := startInstance(t, name, "a", p, store, 4)

	deadline := time.Now().Add(30 * time.Second)
	sawStall := false
	for time.Now().Before(deadline) && !sawStall {
		if s, found, err := postgres.NewCheckpoints(pool).LoadStatus(t.Context(), name); err == nil && found && s.Phase == projection.Stalled {
			sawStall = s.LastError != ""
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !sawStall {
		t.Error("never saw the projection stalled with an error while its handler failed")
	}

	head, _ := store.Head(t.Context())
	waitLive(t, a.runner, head)
	checkTotals(t, p.table, expected(t, store))
}

func TestNewRunner_PartitionsRequireAPartitionedCheckpointStore(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewRunner did not panic without a PartitionedCheckpointStore")
		}
	}()
	p := &totals{}
	projection.NewRunner("x", p.handlers(), memory.NewMemoryStore(0),
		projection.WithPartitions(4, projection.ByStreamID, postgres.NewLeases(pool)))
}
