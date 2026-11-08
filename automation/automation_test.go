package automation_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/memory"
)

// memTodoList is an in-memory todo list whose items are string IDs.
type memTodoList struct {
	mu     sync.Mutex
	leased map[string]time.Time // pending items and their lease end
}

func newTodoList() *memTodoList {
	return &memTodoList{leased: map[string]time.Time{}}
}

func (l *memTodoList) Add(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.leased[id]; !ok {
		l.leased[id] = time.Time{}
	}
	return nil
}

func (l *memTodoList) Complete(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.leased, id)
	return nil
}

func (l *memTodoList) Claim(_ context.Context, req automation.ClaimRequest) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	ids := make([]string, 0, len(l.leased))
	for id, until := range l.leased {
		if !until.After(now) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > req.MaxPerKey {
		ids = ids[:req.MaxPerKey]
	}
	for _, id := range ids {
		l.leased[id] = now.Add(req.Lease)
	}
	return ids, nil
}

func (l *memTodoList) pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.leased)
}

func TestMemTodoList_ClaimAcceptance(t *testing.T) {
	automationtest.ClaimAcceptanceTest(t, func(*testing.T) automationtest.TodoList { return newTodoList() })
}

// dep is a Dependency whose liveness the test sets, and a Watcher.
type dep struct {
	name     string
	live     atomic.Bool
	mu       sync.Mutex
	watchers []func()
}

func newDep(name string, live bool) *dep {
	d := &dep{name: name}
	d.live.Store(live)
	return d
}

func (d *dep) Name() string                         { return d.name }
func (d *dep) IsLive(context.Context) (bool, error) { return d.live.Load(), nil }
func (d *dep) Watch(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watchers = append(d.watchers, fn)
}

func (d *dep) notify() {
	d.mu.Lock()
	ws := append([]func(){}, d.watchers...)
	d.mu.Unlock()
	for _, fn := range ws {
		fn()
	}
}

// quiet discards the automation's log.
var quiet = slog.New(slog.DiscardHandler)

// start runs a and returns a func that stops it and waits for Run.
func start(t *testing.T, a interface{ Run(context.Context) error }) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = a.Run(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("automation did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// completing returns work that completes each item on list and records the
// causation it ran under.
// completing returns work that completes the item, recording the chain its
// ctx carries as "item=correlation/causation" when chains is not nil.
func completing(list *memTodoList, chains *atomic.Value) automation.WorkFunc[string] {
	return func(ctx context.Context, id string) error {
		if chains != nil {
			chains.Store(id + "=" + eventsourcing.CorrelationIDFromContext(ctx) + "/" + eventsourcing.CausationIDFromContext(ctx))
		}
		return list.Complete(ctx, id)
	}
}

func TestRun_ProcessesTheTodoList(t *testing.T) {
	list := newTodoList()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		list.Add(t.Context(), id)
	}
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   newDep("orders-to-ship", true),
		Store:        memory.NewAutomationStore(),
		TodoList:     list,
		Work:         completing(list, nil),
		BatchSize:    2,
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "an empty todo list", func() bool { return list.pending() == 0 })
	eventually(t, "the Idle phase", func() bool { return a.Status().Phase == automation.Idle })
	if st := a.Status(); st.Processed != 5 || st.Failed != 0 {
		t.Fatalf("Status() = %+v, want 5 processed and none failed", st)
	}
}

// TestRun_WorkStartsWithNoChain covers that each item's work starts with
// no correlation or causation, whatever chain the context the runner was
// started with carries, so that items do not share a chain by accident.
func TestRun_WorkStartsWithNoChain(t *testing.T) {
	list := newTodoList()
	list.Add(t.Context(), "a")
	var chain atomic.Value
	a := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work:       completing(list, &chain),
		Logger:     quiet,
	})
	ctx := eventsourcing.WithCausationID(eventsourcing.WithCorrelationID(t.Context(), "outer"), "outer-event")
	if _, err := a.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if got, _ := chain.Load().(string); got != "a=/" {
		t.Fatalf("work ran with chain %q, want none", got)
	}
}

func TestRun_WaitsForEveryDependency(t *testing.T) {
	list := newTodoList()
	list.Add(t.Context(), "a")
	todo := newDep("orders-to-ship", true)
	other := newDep("customers", false)
	var claims atomic.Int32
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   todo,
		WaitFor:      []automation.Dependency{other},
		Store:        memory.NewAutomationStore(),
		TodoList:     countingList{list, &claims},
		Work:         completing(list, nil),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "the Waiting phase", func() bool {
		st := a.Status()
		return st.Phase == automation.Waiting && st.WaitingFor == "customers"
	})
	time.Sleep(50 * time.Millisecond)
	if n := claims.Load(); n != 0 {
		t.Fatalf("claimed %d times while a dependency was not live, want 0", n)
	}

	other.live.Store(true)
	eventually(t, "the item processed", func() bool { return list.pending() == 0 })
}

// TestRun_ChecksLivenessBeforeEveryClaim covers that a todo list that stops
// being live stops the claims, not only one that is not live at start.
func TestRun_ChecksLivenessBeforeEveryClaim(t *testing.T) {
	list := newTodoList()
	todo := newDep("orders-to-ship", true)
	var claims atomic.Int32
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   todo,
		Store:        memory.NewAutomationStore(),
		TodoList:     countingList{list, &claims},
		Work:         completing(list, nil),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)
	eventually(t, "a first claim", func() bool { return claims.Load() > 0 })

	todo.live.Store(false) // a rebuild starts
	eventually(t, "the Waiting phase", func() bool { return a.Status().Phase == automation.Waiting })
	before := claims.Load()
	list.Add(t.Context(), "a")
	time.Sleep(50 * time.Millisecond)
	if n := claims.Load(); n != before || list.pending() != 1 {
		t.Fatalf("claimed %d more times while the todo list was rebuilding, want none", n-before)
	}
}

func TestRun_FailedWorkIsRetriedAfterItsLease(t *testing.T) {
	list := newTodoList()
	list.Add(t.Context(), "a")
	var attempts atomic.Int32
	a := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work: func(ctx context.Context, id string) error {
			if attempts.Add(1) == 1 {
				return errors.New("carrier unavailable")
			}
			return list.Complete(ctx, id)
		},
		Lease:        50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "the retried item processed", func() bool { return list.pending() == 0 })
	st := a.Status()
	if st.Failed != 1 || st.Processed != 1 || !strings.Contains(st.LastError, "carrier unavailable") {
		t.Fatalf("Status() = %+v, want one failure, one success and the failure's error", st)
	}
}

func TestRun_ClaimErrorStalls(t *testing.T) {
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   newDep("orders-to-ship", true),
		Store:        memory.NewAutomationStore(),
		TodoList:     failingList{},
		Work:         func(context.Context, string) error { return nil },
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "the Stalled phase", func() bool { return a.Status().Phase == automation.Stalled })
	if got := a.Status().LastError; !strings.Contains(got, "database down") {
		t.Fatalf("LastError = %q, want the claim's error", got)
	}
}

// TestRun_WatcherNudges covers that a dependency's change wakes an idle
// automation long before its interval.
func TestRun_WatcherNudges(t *testing.T) {
	list := newTodoList()
	todo := newDep("orders-to-ship", true)
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   todo,
		Store:        memory.NewAutomationStore(),
		TodoList:     list,
		Work:         completing(list, nil),
		PollInterval: time.Hour,
		Logger:       quiet,
	})
	start(t, a)
	eventually(t, "the Idle phase", func() bool { return a.Status().Phase == automation.Idle })

	list.Add(t.Context(), "a")
	todo.notify() // the todo list committed new events
	eventually(t, "the item processed after the nudge", func() bool { return list.pending() == 0 })
}

// countingList wraps a todo list and counts its claims.
type countingList struct {
	*memTodoList
	claims *atomic.Int32
}

func (l countingList) Claim(ctx context.Context, req automation.ClaimRequest) ([]string, error) {
	l.claims.Add(1)
	return l.memTodoList.Claim(ctx, req)
}

// failingList is a todo list whose database is down. It counts its claims
// when calls is set.
type failingList struct{ calls *atomic.Int32 }

func (l failingList) Claim(context.Context, automation.ClaimRequest) ([]string, error) {
	if l.calls != nil {
		l.calls.Add(1)
	}
	return nil, errors.New("database down")
}

// job is an item of a keyed todo list: it belongs to a segment (an
// integration) and to a chain (an order) within it.
type job struct {
	ID, Key, Group, Serial string
}

// keyedList is an in-memory todo list with segment keys, the shape the
// default scheduler needs when the list is Segmented.
type keyedList struct {
	mu    sync.Mutex
	seq   int
	items map[string]*keyedItem
}

type keyedItem struct {
	job   job
	seq   int
	lease time.Time
	done  bool
}

func newKeyedList() *keyedList { return &keyedList{items: map[string]*keyedItem{}} }

func (l *keyedList) Add(j job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	l.items[j.ID] = &keyedItem{job: j, seq: l.seq}
}

func (l *keyedList) Complete(_ context.Context, j job) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if it := l.items[j.ID]; it != nil {
		it.done = true
	}
	return nil
}

// Segments reports every segment with an unleased pending item: the list
// is segmented by Query.
func (l *keyedList) Segments() automation.Segmenter { return automation.Query(l.pendingSegments) }

// ClaimSegments is Claim grouped by key, in claim order.
func (l *keyedList) ClaimSegments(ctx context.Context, req automation.ClaimRequest) ([]automation.Claimed[job], error) {
	items, err := l.Claim(ctx, req)
	if err != nil {
		return nil, err
	}
	index := map[string]int{}
	var out []automation.Claimed[job]
	for _, j := range items {
		i, ok := index[j.Key]
		if !ok {
			i = len(out)
			index[j.Key] = i
			out = append(out, automation.Claimed[job]{Segment: j.Key})
		}
		out[i].Items = append(out[i].Items, j)
	}
	return out, nil
}

func (l *keyedList) pendingSegments(context.Context) ([]automation.Segment, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	seen := map[string]bool{}
	var keys []automation.Segment
	for _, it := range l.items {
		if it.done || it.lease.After(now) || seen[it.job.Key] {
			continue
		}
		seen[it.job.Key] = true
		keys = append(keys, automation.Segment{Key: it.job.Key, Group: it.job.Group})
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key < keys[j].Key })
	return keys, nil
}

// Claim leases up to req.MaxPerKey pending items per key in req.Segments,
// in insertion order; all keys when req.Segments is empty.
func (l *keyedList) Claim(_ context.Context, req automation.ClaimRequest) ([]job, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	want := map[string]bool{}
	for _, k := range req.Segments {
		want[k] = true
	}
	var pending []*keyedItem
	for _, it := range l.items {
		if it.done || it.lease.After(now) || (len(want) > 0 && !want[it.job.Key]) {
			continue
		}
		pending = append(pending, it)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].seq < pending[j].seq })
	perKey := map[string]int{}
	var out []job
	for _, it := range pending {
		if perKey[it.job.Key] >= req.MaxPerKey {
			continue
		}
		perKey[it.job.Key]++
		it.lease = now.Add(req.Lease)
		out = append(out, it.job)
	}
	return out, nil
}

func (l *keyedList) pending() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, it := range l.items {
		if !it.done {
			n++
		}
	}
	return n
}

// recorder notes when each item's work started and ended, and the most
// items in flight at once, per chain and overall.
type recorder struct {
	mu       sync.Mutex
	started  []string
	inFlight map[string]int
	maxAll   int
	all      int
	overlaps []string // chains that had two items in flight at once
}

func newRecorder() *recorder { return &recorder{inFlight: map[string]int{}} }

func (r *recorder) work(hold time.Duration) automation.WorkFunc[job] {
	return func(ctx context.Context, j job) error {
		r.mu.Lock()
		r.started = append(r.started, j.ID)
		r.inFlight[j.Serial]++
		if r.inFlight[j.Serial] > 1 {
			r.overlaps = append(r.overlaps, j.Serial)
		}
		r.all++
		if r.all > r.maxAll {
			r.maxAll = r.all
		}
		r.mu.Unlock()
		select {
		case <-time.After(hold):
		case <-ctx.Done():
		}
		r.mu.Lock()
		r.inFlight[j.Serial]--
		r.all--
		r.mu.Unlock()
		return nil
	}
}

func (r *recorder) order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.started...)
}

func TestRunOnce_DrainsThenReturns(t *testing.T) {
	list := newTodoList()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		list.Add(t.Context(), id)
	}
	r := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work:       completing(list, nil),
		BatchSize:  2,
		Logger:     quiet,
	})
	sum, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Processed != 5 || sum.Failed != 0 || sum.Deferred != 0 {
		t.Fatalf("summary = %+v, want 5 processed", sum)
	}
	if n := list.pending(); n != 0 {
		t.Fatalf("%d items left pending", n)
	}
	if got := r.Status(); got.Phase != automation.Idle || got.Processed != 5 {
		t.Fatalf("status = %+v, want idle with 5 processed", got)
	}
}

func TestRunOnce_SchedulerErrorEndsTheRun(t *testing.T) {
	r := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   failingList{},
		Work:       func(context.Context, string) error { return nil },
		Logger:     quiet,
	})
	_, err := r.RunOnce(t.Context())
	if err == nil || !strings.Contains(err.Error(), "database down") {
		t.Fatalf("err = %v, want the claim's error", err)
	}
	if got := r.Status().Phase; got != automation.Stalled {
		t.Fatalf("phase = %v, want stalled", got)
	}
}

// TestOrderingKey_OrdersChainsAndRunsThemInParallel covers the dispatch
// rules: items of one chain run one at a time in claim order, different
// chains fill Concurrency.
func TestOrderingKey_OrdersChainsAndRunsThemInParallel(t *testing.T) {
	list := newKeyedList()
	for _, id := range []string{"a1", "b1", "a2", "b2", "a3", "b3"} {
		list.Add(job{ID: id, Serial: id[:1]})
	}
	rec := newRecorder()
	work := rec.work(15 * time.Millisecond)
	r := automation.NewRunner(automation.Definition[job]{
		Name:        "sync",
		Projection:  newDep("todo", true),
		Store:       memory.NewAutomationStore(),
		TodoList:    list,
		Work:        func(ctx context.Context, j job) error { defer list.Complete(ctx, j); return work(ctx, j) },
		OrderingKey: func(j job) string { return j.Serial },
		Concurrency: 4,
		BatchSize:   10,
		Logger:      quiet,
	})
	if _, err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(rec.overlaps) > 0 {
		t.Fatalf("chains %v had two items in flight at once", rec.overlaps)
	}
	if rec.maxAll < 2 {
		t.Fatalf("at most %d items in flight; chains did not run in parallel", rec.maxAll)
	}
	order := rec.order()
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	for _, chain := range [][]string{{"a1", "a2", "a3"}, {"b1", "b2", "b3"}} {
		for i := 1; i < len(chain); i++ {
			if pos[chain[i]] < pos[chain[i-1]] {
				t.Fatalf("%s started before %s: order %v", chain[i], chain[i-1], order)
			}
		}
	}
}

// TestFailedItem_ParksItsChain covers that a failed item stops later items
// of the same chain for the rest of the batch, while other chains go on.
func TestFailedItem_ParksItsChain(t *testing.T) {
	list := newKeyedList()
	list.Add(job{ID: "a1", Serial: "a"})
	list.Add(job{ID: "a2", Serial: "a"})
	list.Add(job{ID: "b1", Serial: "b"})
	var ran []string
	var mu sync.Mutex
	r := automation.NewRunner(automation.Definition[job]{
		Name:       "sync",
		Projection: newDep("todo", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work: func(ctx context.Context, j job) error {
			mu.Lock()
			ran = append(ran, j.ID)
			mu.Unlock()
			if j.ID == "a1" {
				return errors.New("provider error")
			}
			return list.Complete(ctx, j)
		},
		OrderingKey: func(j job) string { return j.Serial },
		Concurrency: 2,
		Lease:       time.Hour, // a1 and a2 stay leased: the run drains
		Logger:      quiet,
	})
	sum, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Processed != 1 || sum.Failed != 1 {
		t.Fatalf("summary = %+v, want 1 processed, 1 failed", sum)
	}
	sort.Strings(ran)
	if strings.Join(ran, ",") != "a1,b1" {
		t.Fatalf("ran %v, want a1 (failed) and b1 only; a2 must wait for a1's lease", ran)
	}
}

// TestDeferred_PausesClaims covers that Deferred pauses the whole
// unsegmented automation, on every instance sharing the store, without
// counting a failure.
func TestDeferred_PausesClaims(t *testing.T) {
	list := newTodoList()
	list.Add(t.Context(), "a")
	list.Add(t.Context(), "b")
	store := memory.NewAutomationStore()
	var claims atomic.Int32
	var deferred atomic.Bool
	def := automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      store,
		TodoList:   countingList{list, &claims},
		Work: func(ctx context.Context, id string) error {
			if deferred.CompareAndSwap(false, true) {
				return automation.Deferred(time.Now().Add(150 * time.Millisecond))
			}
			return list.Complete(ctx, id)
		},
		BatchSize:    1,
		Lease:        50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	}
	a := automation.NewRunner(def)
	b := automation.NewRunner(def) // a second instance, same store
	start(t, a)
	start(t, b)

	eventually(t, "the deferral", func() bool { return a.Status().Deferred+b.Status().Deferred == 1 })
	before := claims.Load()
	time.Sleep(80 * time.Millisecond)
	if got := claims.Load(); got != before {
		t.Fatalf("%d claims during the pause, want none on either instance", got-before)
	}
	if d, _ := store.DeferredFor(t.Context(), "ship-orders", ""); d <= 0 {
		t.Fatal("store does not report the pause")
	}
	eventually(t, "the list to drain after the pause", func() bool { return list.pending() == 0 })
	if f := a.Status().Failed + b.Status().Failed; f != 0 {
		t.Fatalf("%d failures counted; a deferral is not a failure", f)
	}
}

// TestLeaseExpiry_BoundsWork covers that Work's context ends with the item
// lease and that an item whose lease ran out while it waited in the batch
// is skipped.
func TestLeaseExpiry_BoundsWork(t *testing.T) {
	list := newTodoList()
	for _, id := range []string{"a", "b", "c"} {
		list.Add(t.Context(), id)
	}
	var deadlines atomic.Int32
	r := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work: func(ctx context.Context, id string) error {
			<-ctx.Done() // slower than the lease
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				deadlines.Add(1)
			}
			return ctx.Err()
		},
		BatchSize:    3,
		Lease:        30 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, r)
	eventually(t, "a deadline on Work's context", func() bool { return deadlines.Load() >= 1 })
	eventually(t, "the skipped items", func() bool {
		return strings.Contains(r.Status().LastError, "lease expired before work started")
	})
}

// TestSegmented_OneOwnerPerKey covers two instances sharing a store:
// every item is processed, and no key is ever worked by both at once.
func TestSegmented_OneOwnerPerKey(t *testing.T) {
	list := newKeyedList()
	for _, k := range []string{"acme:shopify", "acme:stripe", "zed:shopify", "zed:stripe"} {
		for i := range 5 {
			list.Add(job{ID: k + "/" + string(rune('0'+i)), Key: k, Group: strings.SplitN(k, ":", 2)[0], Serial: k})
		}
	}
	store := memory.NewAutomationStore()
	var (
		mu       sync.Mutex
		inFlight = map[string]string{} // key → instance working it
		clashes  []string
		byInst   = map[string]int{}
	)
	def := func(inst string) automation.Definition[job] {
		return automation.Definition[job]{
			Name:       "sync",
			Projection: newDep("todo", true),
			Store:      store,
			TodoList:   list,
			Work: func(ctx context.Context, j job) error {
				mu.Lock()
				if other, busy := inFlight[j.Key]; busy && other != inst {
					clashes = append(clashes, j.Key)
				}
				inFlight[j.Key] = inst
				byInst[inst]++
				mu.Unlock()
				time.Sleep(3 * time.Millisecond)
				mu.Lock()
				delete(inFlight, j.Key)
				mu.Unlock()
				return list.Complete(ctx, j)
			},
			MaxSegments:  1,
			BatchSize:    2,
			PollInterval: 5 * time.Millisecond,
			Logger:       quiet,
		}
	}
	a := automation.NewRunner(def("a"))
	b := automation.NewRunner(def("b"))
	start(t, a)
	start(t, b)
	eventually(t, "every item processed", func() bool { return list.pending() == 0 })
	// Work completes an item before it returns, and the runner counts it
	// after; wait for the counts to catch up.
	eventually(t, "every item counted", func() bool { return a.Status().Processed+b.Status().Processed >= 20 })

	mu.Lock()
	defer mu.Unlock()
	if len(clashes) > 0 {
		t.Fatalf("keys worked by both instances at once: %v", clashes)
	}
	if byInst["a"]+byInst["b"] != 20 {
		t.Fatalf("processed %v, want 20 in total", byInst)
	}
	if got := a.Status().Processed + b.Status().Processed; got != 20 {
		t.Fatalf("status counts %d, want 20", got)
	}
}

// TestSegmented_DeferredPausesOnlyThatKey covers that a 429 from one
// integration pauses that segment and no other.
func TestSegmented_DeferredPausesOnlyThatKey(t *testing.T) {
	list := newKeyedList()
	for i := range 3 {
		list.Add(job{ID: "slow/" + string(rune('0'+i)), Key: "slow"})
		list.Add(job{ID: "fast/" + string(rune('0'+i)), Key: "fast"})
	}
	var deferred atomic.Bool
	r := automation.NewRunner(automation.Definition[job]{
		Name:       "sync",
		Projection: newDep("todo", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work: func(ctx context.Context, j job) error {
			if j.Key == "slow" && deferred.CompareAndSwap(false, true) {
				return automation.Deferred(time.Now().Add(200 * time.Millisecond))
			}
			return list.Complete(ctx, j)
		},
		Lease:        50 * time.Millisecond,
		PollInterval: 5 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, r)

	eventually(t, "fast items processed while slow is paused", func() bool {
		list.mu.Lock()
		defer list.mu.Unlock()
		fastLeft, slowLeft := 0, 0
		for _, it := range list.items {
			if !it.done {
				if it.job.Key == "fast" {
					fastLeft++
				} else {
					slowLeft++
				}
			}
		}
		return fastLeft == 0 && slowLeft == 3
	})
	eventually(t, "slow items processed after the pause", func() bool { return list.pending() == 0 })
	if s := r.Status(); s.Deferred != 1 || s.Failed != 0 {
		t.Fatalf("status = %+v, want one deferral and no failure", s)
	}
}

// TestNewRunner_Panics covers each rejected definition. Every case has a
// valid store except "list without store", so the panic names the field
// that is actually missing.
func TestNewRunner_Panics(t *testing.T) {
	dep := newDep("todo", true)
	work := func(context.Context, string) error { return nil }
	list := newTodoList()
	store := memory.NewAutomationStore()
	sched := nopScheduler[string]{}
	cases := map[string]struct {
		def  automation.Definition[string]
		want string
	}{
		"no name":                  {automation.Definition[string]{Projection: dep, Work: work, TodoList: list, Store: store}, "Name is required"},
		"no projection":            {automation.Definition[string]{Name: "x", Work: work, TodoList: list, Store: store}, "Projection is required"},
		"no work":                  {automation.Definition[string]{Name: "x", Projection: dep, TodoList: list, Store: store}, "Work is required"},
		"no list and no scheduler": {automation.Definition[string]{Name: "x", Projection: dep, Work: work, Store: store}, "TodoList or Scheduler is required"},
		"list and scheduler":       {automation.Definition[string]{Name: "x", Projection: dep, Work: work, TodoList: list, Store: store, Scheduler: sched}, "Scheduler excludes TodoList and Store"},
		"scheduler and store":      {automation.Definition[string]{Name: "x", Projection: dep, Work: work, Scheduler: sched, Store: store}, "Scheduler excludes TodoList and Store"},
		"list without store":       {automation.Definition[string]{Name: "x", Projection: dep, Work: work, TodoList: list}, "Store is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				got := fmt.Sprint(recover())
				if !strings.Contains(got, tc.want) {
					t.Fatalf("NewRunner panicked with %q, want %q", got, tc.want)
				}
			}()
			automation.NewRunner(tc.def)
		})
	}
}

// TestSegmented_ConcurrentDeferrals covers several segments deferring at
// the same moment, as when every tenant's integration answers 429: their
// batches run in parallel, so the scheduler hears of the deferrals from
// several goroutines at once. Run it with -race.
func TestSegmented_ConcurrentDeferrals(t *testing.T) {
	const segments = 8
	list := newKeyedList()
	for i := range segments {
		key := "tenant-" + string(rune('a'+i))
		list.Add(job{ID: key + "/1", Key: key})
	}
	store := memory.NewAutomationStore()
	ready := make(chan struct{})
	var arrived atomic.Int32
	r := automation.NewRunner(automation.Definition[job]{
		Name:       "sync",
		Projection: newDep("todo", true),
		Store:      store,
		TodoList:   list,
		Work: func(ctx context.Context, j job) error {
			// Hold every batch until all have started, so that the
			// deferrals are reported together.
			if arrived.Add(1) == segments {
				close(ready)
			}
			select {
			case <-ready:
			case <-ctx.Done():
				return ctx.Err()
			}
			return automation.Deferred(time.Now().Add(time.Hour))
		},
		Lease:        time.Second,
		PollInterval: 5 * time.Millisecond,
		MaxSegments:  segments,
		Logger:       quiet,
	})
	sum, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Deferred != segments {
		t.Fatalf("summary = %+v, want %d deferrals", sum, segments)
	}
	for i := range segments {
		key := "tenant-" + string(rune('a'+i))
		if d, _ := store.DeferredFor(t.Context(), "sync", key); d < 50*time.Minute {
			t.Fatalf("segment %s paused for %s, want about an hour", key, d)
		}
	}
}

// nopScheduler is a Scheduler that never has work, for wiring tests and
// as the smallest example of the seam.
type nopScheduler[T any] struct{}

func (nopScheduler[T]) Next(context.Context) ([]automation.Batch[T], error) { return nil, nil }
func (nopScheduler[T]) Done(context.Context, automation.Item[T], automation.Outcome) error {
	return nil
}
func (nopScheduler[T]) Drained(context.Context, string) error { return nil }
func (nopScheduler[T]) Close(context.Context) error           { return nil }

// TestCustomScheduler covers that a Scheduler replaces the default
// claiming entirely: the runner works what it hands out and reports back.
func TestCustomScheduler(t *testing.T) {
	s := &scriptedScheduler{batches: [][]automation.Batch[string]{
		{{Key: "k1", Items: []automation.Item[string]{{Key: "k1", Value: "a"}, {Key: "k1", Value: "b"}}}},
		{{Key: "k2", Items: []automation.Item[string]{{Key: "k2", Value: "c"}}}},
	}}
	var worked []string
	var mu sync.Mutex
	r := automation.NewRunner(automation.Definition[string]{
		Name:       "custom",
		Projection: newDep("todo", true),
		Scheduler:  s,
		Work: func(_ context.Context, v string) error {
			mu.Lock()
			worked = append(worked, v)
			mu.Unlock()
			if v == "b" {
				return errors.New("nope")
			}
			return nil
		},
		Logger: quiet,
	})
	sum, err := r.RunOnce(t.Context())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sum.Processed != 2 || sum.Failed != 1 {
		t.Fatalf("summary = %+v, want 2 processed, 1 failed", sum)
	}
	sort.Strings(worked)
	if strings.Join(worked, ",") != "a,b,c" {
		t.Fatalf("worked %v, want a, b and c", worked)
	}
	if !s.closed {
		t.Fatal("scheduler was not closed")
	}
	if len(s.outcomes) != 3 || s.outcomes["b"].Kind != automation.Failed {
		t.Fatalf("outcomes = %+v, want three with b failed", s.outcomes)
	}
	sort.Strings(s.drained)
	if strings.Join(s.drained, ",") != "k1,k2" {
		t.Fatalf("drained %v, want k1 and k2 once each", s.drained)
	}
}

type scriptedScheduler struct {
	mu       sync.Mutex
	batches  [][]automation.Batch[string]
	outcomes map[string]automation.Outcome
	drained  []string
	closed   bool
}

func (s *scriptedScheduler) Next(context.Context) ([]automation.Batch[string], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.batches) == 0 {
		return nil, nil
	}
	b := s.batches[0]
	s.batches = s.batches[1:]
	return b, nil
}

func (s *scriptedScheduler) Done(_ context.Context, it automation.Item[string], out automation.Outcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.outcomes == nil {
		s.outcomes = map[string]automation.Outcome{}
	}
	s.outcomes[it.Value] = out
	return nil
}

func (s *scriptedScheduler) Drained(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drained = append(s.drained, key)
	return nil
}

func (s *scriptedScheduler) Close(context.Context) error { s.closed = true; return nil }

// TestSegmented_SlowSegmentDoesNotHoldOthers covers that batches run on
// their own: while one segment's batch is stuck, another segment works
// through batch after batch.
func TestSegmented_SlowSegmentDoesNotHoldOthers(t *testing.T) {
	list := newKeyedList()
	list.Add(job{ID: "slow/1", Key: "slow"})
	for i := range 5 {
		list.Add(job{ID: "fast/" + string(rune('1'+i)), Key: "fast"})
	}
	release := make(chan struct{})
	r := automation.NewRunner(automation.Definition[job]{
		Name:       "sync",
		Projection: newDep("todo", true),
		Store:      memory.NewAutomationStore(),
		TodoList:   list,
		Work: func(ctx context.Context, j job) error {
			if j.Key == "slow" {
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return list.Complete(ctx, j)
		},
		BatchSize:    1, // one item per batch: fast needs five batches
		Lease:        10 * time.Second,
		PollInterval: time.Hour, // only finishing batches wake the runner
		Logger:       quiet,
	})
	start(t, r)

	eventually(t, "fast drained while slow is stuck", func() bool { return list.pending() == 1 })
	close(release)
	eventually(t, "slow processed once released", func() bool { return list.pending() == 0 })
}

// TestRun_PauseThroughTheStore covers pausing and resuming an automation
// through its store, as a dashboard does: paused, it claims nothing and
// publishes the Paused phase; resumed, it works the list off.
func TestRun_PauseThroughTheStore(t *testing.T) {
	ctx := t.Context()
	store := memory.NewAutomationStore()
	if err := store.SetPaused(ctx, "ship-orders", true); err != nil {
		t.Fatal(err)
	}
	list := newTodoList()
	list.Add(ctx, "a")
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   newDep("orders-to-ship", true),
		Store:        store,
		TodoList:     list,
		Work:         completing(list, nil),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "the Paused phase", func() bool { return a.Status().Phase == automation.Paused })
	eventually(t, "Paused published", func() bool {
		sts, _ := store.LoadStatuses(ctx, "ship-orders")
		return len(sts) == 1 && sts[0].Phase == automation.Paused && sts[0].Owner == a.Status().Owner
	})
	time.Sleep(50 * time.Millisecond) // several polls
	if list.pending() != 1 {
		t.Fatal("a paused automation claimed work")
	}

	if err := store.SetPaused(ctx, "ship-orders", false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the item worked after resuming", func() bool { return list.pending() == 0 })
	eventually(t, "Idle published", func() bool {
		sts, _ := store.LoadStatuses(ctx, "ship-orders")
		return len(sts) == 1 && sts[0].Phase == automation.Idle && sts[0].Processed == 1
	})
}

// publishingStore wraps a store and records every status it was asked to
// save, with the ttl and the time of the write.
type publishingStore struct {
	automation.Store
	mu     sync.Mutex
	writes []statusWrite
}

type statusWrite struct {
	status automation.Status
	ttl    time.Duration
	at     time.Time
}

func (s *publishingStore) SaveStatus(ctx context.Context, name string, st automation.Status, ttl time.Duration) error {
	s.mu.Lock()
	s.writes = append(s.writes, statusWrite{status: st, ttl: ttl, at: time.Now()})
	s.mu.Unlock()
	return s.Store.SaveStatus(ctx, name, st, ttl)
}

func (s *publishingStore) written() []statusWrite {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]statusWrite{}, s.writes...)
}

// published returns the phase the store shows for the one instance of
// name, or -1 when it shows none.
func published(t *testing.T, store automation.Store, name string) automation.Phase {
	t.Helper()
	sts, err := store.LoadStatuses(t.Context(), name)
	if err != nil {
		t.Fatalf("LoadStatuses: %v", err)
	}
	if len(sts) != 1 {
		return -1
	}
	return sts[0].Phase
}

// TestRun_PublishesPhaseChangesAtOnce covers that a new phase reaches the
// store before the runner waits for its next poll, not after: the poll
// interval is an hour, so a phase that is only published at the top of
// the next round never shows up.
func TestRun_PublishesPhaseChangesAtOnce(t *testing.T) {
	ctx := t.Context()
	store := memory.NewAutomationStore()
	todo := newDep("orders-to-ship", true)
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   todo,
		Store:        store,
		TodoList:     newTodoList(),
		Work:         func(context.Context, string) error { return nil },
		PollInterval: time.Hour,
		Logger:       quiet,
	})
	start(t, a)
	eventually(t, "Idle published", func() bool { return published(t, store, "ship-orders") == automation.Idle })

	todo.live.Store(false)
	a.Nudge()
	eventually(t, "Waiting published", func() bool { return published(t, store, "ship-orders") == automation.Waiting })

	todo.live.Store(true)
	if err := store.SetPaused(ctx, "ship-orders", true); err != nil {
		t.Fatal(err)
	}
	a.Nudge()
	eventually(t, "Paused published", func() bool { return published(t, store, "ship-orders") == automation.Paused })

	if err := store.SetPaused(ctx, "ship-orders", false); err != nil {
		t.Fatal(err)
	}
	a.Nudge()
	eventually(t, "Idle published after resuming", func() bool { return published(t, store, "ship-orders") == automation.Idle })
}

// TestRun_StalledStatusOutlivesTheBackoff covers a stalled runner: the
// Stalled phase is published before the backoff sleep, and the status is
// live for that sleep on top of the usual ttl, so the instance does not
// drop out of LoadStatuses while it backs off.
func TestRun_StalledStatusOutlivesTheBackoff(t *testing.T) {
	store := &publishingStore{Store: memory.NewAutomationStore()}
	claims := &atomic.Int32{}
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   newDep("orders-to-ship", true),
		Store:        store,
		TodoList:     failingList{calls: claims},
		Work:         func(context.Context, string) error { return nil },
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	eventually(t, "Stalled published", func() bool { return published(t, store, "ship-orders") == automation.Stalled })
	if n := claims.Load(); n != 1 {
		t.Fatalf("Stalled was published after %d claims, want before the retry", n)
	}

	// The write after the Stalled one comes once the backoff has slept.
	var stalled int
	eventually(t, "the publish after the backoff", func() bool {
		ws := store.written()
		for i, w := range ws {
			if w.status.Phase == automation.Stalled {
				stalled = i
				return len(ws) > i+1
			}
		}
		return false
	})
	ws := store.written()
	usual := ws[0].ttl // published while Starting
	slept := ws[stalled+1].at.Sub(ws[stalled].at)
	if got := ws[stalled].ttl; got < usual+slept-100*time.Millisecond {
		t.Fatalf("Stalled status ttl = %s, want the usual %s plus the %s backoff", got, usual, slept)
	}
}

// TestRun_WithdrawsItsStatusWhenStopped covers that a stopped instance
// leaves LoadStatuses at once rather than when its last status expires.
func TestRun_WithdrawsItsStatusWhenStopped(t *testing.T) {
	store := memory.NewAutomationStore()
	a := automation.NewRunner(automation.Definition[string]{
		Name:         "ship-orders",
		Projection:   newDep("orders-to-ship", true),
		Store:        store,
		TodoList:     newTodoList(),
		Work:         func(context.Context, string) error { return nil },
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	stop := start(t, a)
	eventually(t, "Idle published", func() bool { return published(t, store, "ship-orders") == automation.Idle })

	stop()
	if got := published(t, store, "ship-orders"); got != -1 {
		t.Fatalf("a stopped instance still shows %s", got)
	}
}

// TestRun_PauseLetsRunningWorkFinish covers pausing while an item is being
// worked: the item finishes, nothing more is claimed, and the segment the
// instance held is given back.
func TestRun_PauseLetsRunningWorkFinish(t *testing.T) {
	ctx := t.Context()
	store := memory.NewAutomationStore()
	list := newKeyedList()
	list.Add(job{ID: "a/1", Key: "a"})
	list.Add(job{ID: "a/2", Key: "a"})
	started, release := make(chan struct{}, 2), make(chan struct{})
	a := automation.NewRunner(automation.Definition[job]{
		Name:       "sync",
		Projection: newDep("todo", true),
		Store:      store,
		TodoList:   list,
		Work: func(ctx context.Context, j job) error {
			started <- struct{}{}
			<-release
			return list.Complete(ctx, j)
		},
		BatchSize:    1,
		Lease:        10 * time.Second,
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet,
	})
	start(t, a)

	<-started
	if err := store.SetPaused(ctx, "sync", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the Paused phase", func() bool { return a.Status().Phase == automation.Paused })
	close(release)
	eventually(t, "the running item finished", func() bool { return list.pending() == 1 })
	eventually(t, "the segment given back", func() bool {
		segs, _ := store.LoadSegments(ctx, "sync")
		return len(segs) == 1 && segs[0].Owner == ""
	})
	select {
	case <-started:
		t.Fatal("a paused automation started another item")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestRunOnce_PausedDoesNothing(t *testing.T) {
	ctx := t.Context()
	store := memory.NewAutomationStore()
	if err := store.SetPaused(ctx, "ship-orders", true); err != nil {
		t.Fatal(err)
	}
	list := newTodoList()
	list.Add(ctx, "a")
	a := automation.NewRunner(automation.Definition[string]{
		Name:       "ship-orders",
		Projection: newDep("orders-to-ship", true),
		Store:      store,
		TodoList:   list,
		Work:       completing(list, nil),
		Logger:     quiet,
	})
	sum, err := a.RunOnce(ctx)
	if err != nil || sum.Processed != 0 || list.pending() != 1 {
		t.Fatalf("RunOnce = %+v, %v with %d pending; want nothing done", sum, err, list.pending())
	}
	if a.Status().Phase != automation.Paused {
		t.Fatalf("phase %s, want paused", a.Status().Phase)
	}
}

// The types of ExampleDefinition: a todo list of orders to ship, and the
// automation that ships them, laid out as the Definition doc describes.
type shippingTodo struct{ OrderID, Address string }

// ordersToShip is the todo list. A projection fills it; Claim leases from
// it.
type ordersToShip struct {
	mu      sync.Mutex
	pending map[string]shippingTodo
	leased  map[string]time.Time // until when each pending item is leased
}

func (l *ordersToShip) Claim(_ context.Context, req automation.ClaimRequest) ([]shippingTodo, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	var out []shippingTodo
	for id, t := range l.pending {
		if len(out) == req.MaxPerKey {
			break
		}
		if l.leased[id].After(now) {
			continue
		}
		l.leased[id] = now.Add(req.Lease)
		out = append(out, t)
	}
	return out, nil
}

// shipOrder is the command the automation sends for one item.
type shipOrder struct{ OrderID string }

func (c shipOrder) AggregateID() string { return c.OrderID }
func (c shipOrder) CommandType() string { return "ShipOrder" }

// shipOrders is the automation as a type: the todo list it claims from, the
// projection that keeps that list, and the dispatcher its commands go to.
type shipOrders struct {
	list *ordersToShip
	todo automation.Dependency
	bus  eventsourcing.Dispatcher
}

func newShipOrders(list *ordersToShip, todo automation.Dependency, bus eventsourcing.Dispatcher) *shipOrders {
	return &shipOrders{list: list, todo: todo, bus: bus}
}

func (a *shipOrders) Definition() automation.Definition[shippingTodo] {
	return automation.Definition[shippingTodo]{
		Name:       "ship-orders",
		TodoList:   a.list,
		Projection: a.todo, // work only while the list is live
		Work:       a.Work,
	}
}

func (a *shipOrders) Work(ctx context.Context, t shippingTodo) error {
	_, err := a.bus.Dispatch(ctx, shipOrder{OrderID: t.OrderID})
	return err
}

// ExampleDefinition is main's side of the Definition doc: the automation
// describes itself, main chooses where its leases live and runs it.
func ExampleDefinition() {
	list := &ordersToShip{pending: map[string]shippingTodo{}, leased: map[string]time.Time{}}
	todoRunner := newDep("orders-to-ship", true) // the *projection.Runner that keeps list
	commandBus := eventsourcing.NewCommandBus(20, 1)
	defer commandBus.Stop()

	ship := newShipOrders(list, todoRunner, commandBus).Definition()
	ship.Store = memory.NewAutomationStore() // one process; sqlite or postgres to share leases
	runner := automation.NewRunner(ship)
	_ = runner.Run // go runner.Run(ctx)
}
