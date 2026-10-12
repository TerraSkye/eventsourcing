//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventstore/memory"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/postgres"
)

func startSingle(t *testing.T, name string, p *totals, store *memory.MemoryStore, opts ...projection.Option) *instance {
	t.Helper()
	opts = append([]projection.Option{
		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
		projection.WithPollInterval(50 * time.Millisecond),
		projection.WithBatchSize(50),
		projection.WithRetryStrategy(backoff.NewConstantBackOff(20 * time.Millisecond)),
		projection.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))),
	}, opts...)
	r := projection.NewRunner(name, p.handlers(), store, opts...)
	ctx, cancel := context.WithCancel(context.Background())
	inst := &instance{runner: r, cancel: cancel, done: make(chan error, 1)}
	go func() { inst.done <- r.Run(ctx) }()
	t.Cleanup(inst.stop)
	return inst
}

func (p *totals) reset(ctx context.Context) error {
	_, err := postgres.Tx(ctx).Exec(ctx, fmt.Sprintf(`TRUNCATE %s`, p.table))
	return err
}

func TestRunner_OneInstanceHoldsTheLockAndFailsOver(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)
	appendOrders(t, store, 4, 400, 25)

	a := startSingle(t, name, p, store)
	head, _ := store.Head(t.Context())
	waitLive(t, a.runner, head)

	b := startSingle(t, name, p, store)
	deadline := time.Now().Add(10 * time.Second)
	for b.runner.Status().Phase != projection.Standby {
		if time.Now().After(deadline) {
			t.Fatalf("b never reported standby: %+v", b.runner.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if live, err := b.runner.Live(t.Context()); err != nil || !live {
		t.Errorf("standby b sees the projection live = %v, %v; want true", live, err)
	}
	remote := projection.Remote(name, postgres.NewCheckpoints(pool), projection.MaxAge(time.Minute))
	if live, err := remote.Live(t.Context()); err != nil || !live {
		t.Errorf("Remote live = %v, %v; want true", live, err)
	}

	a.stop()
	appendOrders(t, store, 4, 400, 25)
	head, _ = store.Head(t.Context())
	waitLive(t, b.runner, head)
	checkTotals(t, p.table, expected(t, store))
	if owner := b.runner.Status().Owner; owner == "" {
		t.Error("active runner reports no owner")
	}
}

func TestRunner_RebuildTruncatesAndReplays(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)
	appendOrders(t, store, 2, 300, 20)

	a := startSingle(t, name, p, store, projection.WithReset(p.reset))
	head, _ := store.Head(t.Context())
	waitLive(t, a.runner, head)

	if err := a.runner.Rebuild(t.Context()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	waitLive(t, a.runner, head)
	checkTotals(t, p.table, expected(t, store))

	// A new version rebuilds on start.
	a.stop()
	b := startSingle(t, name, p, store, projection.WithReset(p.reset), projection.WithVersion(2))
	// Until b is active, WaitUntil and Live answer from the status a left
	// behind, so wait for b itself.
	deadline := time.Now().Add(30 * time.Second)
	for s := b.runner.Status(); s.Phase != projection.Live || s.Version != 2 || s.Position < head; s = b.runner.Status() {
		if time.Now().After(deadline) {
			t.Fatalf("b never went live at version 2: %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
	checkTotals(t, p.table, expected(t, store))
	if s, _, _ := postgres.NewCheckpoints(pool).LoadStatus(t.Context(), name); s.Version != 2 {
		t.Errorf("stored version = %d, want 2", s.Version)
	}
}

func TestRunner_FailingBatchRollsBackAndSkipsToDeadLetter(t *testing.T) {
	store := memory.NewMemoryStore(0)
	p := newTotals(t)
	name := uniqueName(t)
	appendOrders(t, store, 1, 120, 10)
	const bad = 77
	p.fail = func(gv uint64) error {
		if gv == bad {
			return errors.New("poison")
		}
		return nil
	}

	a := startSingle(t, name, p, store, projection.WithErrorPolicy(projection.SkipAlways))
	head, _ := store.Head(t.Context())
	waitLive(t, a.runner, head)

	want := expected(t, store)
	for _, w := range want {
		if w.seen[bad] {
			w.items--
			w.total -= amountOf(t, store, bad)
			delete(w.seen, bad)
		}
	}
	checkTotals(t, p.table, want)

	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM projection_dead_letters WHERE projection = $1 AND global_version = $2 AND error = 'poison'`,
		name, bad).Scan(&n); err != nil || n != 1 {
		t.Errorf("dead letters for event %d = %d, %v; want 1", bad, n, err)
	}
	if s := a.runner.Status(); s.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", s.Skipped)
	}
}

func amountOf(t *testing.T, store *memory.MemoryStore, gv uint64) int {
	t.Helper()
	res, err := store.ReadAll(t.Context(), gv-1, eventsourcing.ReadOptions{Limit: 1})
	if err != nil || len(res.Events) == 0 {
		t.Fatalf("read event %d: %v", gv, err)
	}
	return res.Events[0].Event.(*ItemAdded).Amount
}

func TestLock_LostWhenItsConnectionDies(t *testing.T) {
	ctx := t.Context()
	cps := postgres.NewCheckpoints(pool)
	name := uniqueName(t)

	lock, ok, err := cps.TryLock(ctx, name)
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	if _, ok, _ := cps.TryLock(ctx, name); ok {
		t.Fatal("second TryLock acquired a held lock")
	}

	if _, err := pool.Exec(ctx, `
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND classid = $1::bigint::oid AND objid = (hashtext($2)::bigint & 4294967295)::oid`,
		0x70726f6a, name); err != nil {
		t.Fatalf("terminate lock backend: %v", err)
	}
	select {
	case <-lock.Lost():
	case <-time.After(10 * time.Second):
		t.Fatal("Lost not closed after the lock's connection was terminated")
	}
	_ = lock.Release(ctx)

	again, ok, err := cps.TryLock(ctx, name)
	if err != nil || !ok {
		t.Fatalf("TryLock after loss = %v, %v", ok, err)
	}
	if err := again.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}
