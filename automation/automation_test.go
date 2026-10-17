package automation_test

import (
	"context"
	"errors"
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

func (l *memTodoList) Claim(_ context.Context, max int, lease time.Duration) ([]string, error) {
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
	if len(ids) > max {
		ids = ids[:max]
	}
	for _, id := range ids {
		l.leased[id] = now.Add(lease)
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
func completing(list *memTodoList, causation *atomic.Value) automation.WorkFunc[string] {
	return func(ctx context.Context, id string) error {
		if causation != nil {
			causation.Store(eventsourcing.CausationFromContext(ctx))
		}
		return list.Complete(ctx, id)
	}
}

func TestRun_ProcessesTheTodoList(t *testing.T) {
	list := newTodoList()
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		list.Add(t.Context(), id)
	}
	var causation atomic.Value
	a := automation.New(automation.Definition[string]{
		Name:      "ship-orders",
		TodoList:  newDep("orders-to-ship", true),
		Claim:     list.Claim,
		Work:      completing(list, &causation),
		BatchSize: 2,
		Interval:  10 * time.Millisecond,
		Logger:    quiet,
	})
	start(t, a)

	eventually(t, "an empty todo list", func() bool { return list.pending() == 0 })
	eventually(t, "the Idle phase", func() bool { return a.Status().Phase == automation.Idle })
	if st := a.Status(); st.Processed != 5 || st.Failed != 0 {
		t.Fatalf("Status() = %+v, want 5 processed and none failed", st)
	}
	if got := causation.Load(); got != "ship-orders" {
		t.Fatalf("work ran with causation %q, want the automation's name", got)
	}
}

func TestRun_WaitsForEveryDependency(t *testing.T) {
	list := newTodoList()
	list.Add(t.Context(), "a")
	todo := newDep("orders-to-ship", true)
	other := newDep("customers", false)
	var claims atomic.Int32
	a := automation.New(automation.Definition[string]{
		Name:     "ship-orders",
		TodoList: todo,
		WaitFor:  []automation.Dependency{other},
		Claim: func(ctx context.Context, max int, lease time.Duration) ([]string, error) {
			claims.Add(1)
			return list.Claim(ctx, max, lease)
		},
		Work:     completing(list, nil),
		Interval: 10 * time.Millisecond,
		Logger:   quiet,
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
	a := automation.New(automation.Definition[string]{
		Name:     "ship-orders",
		TodoList: todo,
		Claim: func(ctx context.Context, max int, lease time.Duration) ([]string, error) {
			claims.Add(1)
			return list.Claim(ctx, max, lease)
		},
		Work:     completing(list, nil),
		Interval: 10 * time.Millisecond,
		Logger:   quiet,
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
	a := automation.New(automation.Definition[string]{
		Name:     "ship-orders",
		TodoList: newDep("orders-to-ship", true),
		Claim:    list.Claim,
		Work: func(ctx context.Context, id string) error {
			if attempts.Add(1) == 1 {
				return errors.New("carrier unavailable")
			}
			return list.Complete(ctx, id)
		},
		Lease:    50 * time.Millisecond,
		Interval: 10 * time.Millisecond,
		Logger:   quiet,
	})
	start(t, a)

	eventually(t, "the retried item processed", func() bool { return list.pending() == 0 })
	st := a.Status()
	if st.Failed != 1 || st.Processed != 1 || !strings.Contains(st.LastError, "carrier unavailable") {
		t.Fatalf("Status() = %+v, want one failure, one success and the failure's error", st)
	}
}

func TestRun_ClaimErrorStalls(t *testing.T) {
	a := automation.New(automation.Definition[string]{
		Name:     "ship-orders",
		TodoList: newDep("orders-to-ship", true),
		Claim: func(context.Context, int, time.Duration) ([]string, error) {
			return nil, errors.New("database down")
		},
		Work:     func(context.Context, string) error { return nil },
		Interval: 10 * time.Millisecond,
		Logger:   quiet,
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
	a := automation.New(automation.Definition[string]{
		Name:     "ship-orders",
		TodoList: todo,
		Claim:    list.Claim,
		Work:     completing(list, nil),
		Interval: time.Hour,
		Logger:   quiet,
	})
	start(t, a)
	eventually(t, "the Idle phase", func() bool { return a.Status().Phase == automation.Idle })

	list.Add(t.Context(), "a")
	todo.notify() // the todo list committed new events
	eventually(t, "the item processed after the nudge", func() bool { return list.pending() == 0 })
}

func TestNew_Panics(t *testing.T) {
	list := newTodoList()
	work := func(context.Context, string) error { return nil }
	tests := map[string]automation.Definition[string]{
		"no name":      {TodoList: newDep("x", true), Claim: list.Claim, Work: work},
		"no todo list": {Name: "a", Claim: list.Claim, Work: work},
		"no claim":     {Name: "a", TodoList: newDep("x", true), Work: work},
		"no work":      {Name: "a", TodoList: newDep("x", true), Claim: list.Claim},
	}
	for name, d := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("New did not panic")
				}
			}()
			automation.New(d)
		})
	}
}
