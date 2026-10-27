package sqlite_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/sqlite"
)

type taskCreated struct {
	ID string
}

func (e *taskCreated) AggregateID() string { return e.ID }
func (e *taskCreated) EventType() string   { return "sqlite_test.TaskCreated" }

type taskDone struct {
	ID string
}

func (e *taskDone) AggregateID() string { return e.ID }
func (e *taskDone) EventType() string   { return "sqlite_test.TaskDone" }

func init() {
	eventsourcing.RegisterEvent(&taskCreated{})
	eventsourcing.RegisterEvent(&taskDone{})
}

// appendEvents saves events to stream id and returns the last position.
func appendEvents(t *testing.T, store *sqlite.EventStore, id string, events ...eventsourcing.Event) uint64 {
	t.Helper()
	envs := make([]eventsourcing.Envelope, len(events))
	for i, e := range events {
		envs[i] = eventsourcing.Envelope{EventID: uuid.New(), StreamID: id, Event: e, OccurredAt: time.Now()}
	}
	res, err := store.Save(t.Context(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return res.GlobalPosition
}

// recorder collects the events a handler receives.
type recorder struct {
	mu     sync.Mutex
	events []eventsourcing.Event
	fail   func(eventsourcing.Event) error
}

func (r *recorder) Handle(ctx context.Context, e eventsourcing.Event) error {
	if r.fail != nil {
		if err := r.fail(e); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

// waitFor fails the test unless cond holds within two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEventBus_DeliversInOrder(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)
	bus := sqlite.NewEventBus(db, 10*time.Millisecond)
	t.Cleanup(func() { bus.Close() })

	rec := &recorder{}
	if err := bus.Subscribe(t.Context(), "all", rec); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"}, &taskDone{ID: "1"})
	appendEvents(t, store, "task-2", &taskCreated{ID: "2"})

	waitFor(t, "three events", func() bool { return rec.count() == 3 })
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.events[1].(*taskDone); !ok {
		t.Fatalf("second event = %T, want *taskDone (id order)", rec.events[1])
	}
}

func TestEventBus_FilterEvents(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)
	bus := sqlite.NewEventBus(db, 10*time.Millisecond)
	t.Cleanup(func() { bus.Close() })

	rec := &recorder{}
	if err := bus.Subscribe(t.Context(), "done", rec, sqlite.WithFilterEvents([]string{"sqlite_test.TaskDone"})); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"}, &taskDone{ID: "1"}, &taskCreated{ID: "1"})

	waitFor(t, "the TaskDone event", func() bool { return rec.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := rec.count(); n != 1 {
		t.Fatalf("received %d events, want only the one TaskDone", n)
	}
}

// TestEventBus_ResumesFromStoredPosition covers that a new bus on the same
// database delivers only what the subscription has not seen.
func TestEventBus_ResumesFromStoredPosition(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)

	first := sqlite.NewEventBus(db, 10*time.Millisecond)
	rec := &recorder{}
	if err := first.Subscribe(t.Context(), "sub", rec); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"}, &taskDone{ID: "1"})
	waitFor(t, "two events", func() bool { return rec.count() == 2 })
	first.Close()

	appendEvents(t, store, "task-2", &taskCreated{ID: "2"})
	second := sqlite.NewEventBus(db, 10*time.Millisecond)
	t.Cleanup(func() { second.Close() })
	again := &recorder{}
	if err := second.Subscribe(t.Context(), "sub", again); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	waitFor(t, "the new event", func() bool { return again.count() == 1 })
	time.Sleep(50 * time.Millisecond)
	if n := again.count(); n != 1 {
		t.Fatalf("resumed subscription received %d events, want 1", n)
	}
}

// TestEventBus_FailingHandlerIsRetried covers that a failed event is
// delivered again, and that the events before it are not.
func TestEventBus_FailingHandlerIsRetried(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)
	bus := sqlite.NewEventBus(db, 10*time.Millisecond)
	t.Cleanup(func() { bus.Close() })

	var mu sync.Mutex
	failures := 0
	rec := &recorder{fail: func(e eventsourcing.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := e.(*taskDone); ok && failures < 2 {
			failures++
			return errors.New("boom")
		}
		return nil
	}}
	if err := bus.Subscribe(t.Context(), "sub", rec); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"}, &taskDone{ID: "1"})

	waitFor(t, "both events", func() bool { return rec.count() == 2 })
	select {
	case err := <-bus.Errors():
		if err == nil {
			t.Fatal("Errors delivered nil")
		}
	default:
		t.Fatal("no error reported for the failing handler")
	}
}

func TestEventBus_HandlerMayUseTheDatabase(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)
	bus := sqlite.NewEventBus(db, 10*time.Millisecond)
	t.Cleanup(func() { bus.Close() })

	done := make(chan struct{})
	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, e eventsourcing.Event) error {
		if _, ok := e.(*taskCreated); !ok {
			return nil
		}
		// Reacting to an event by appending another one.
		appendEvents(t, store, "task-1", &taskDone{ID: "1"})
		close(done)
		return nil
	})
	if err := bus.Subscribe(t.Context(), "reactor", handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler that appends an event never finished")
	}
}

func TestEventBus_CloseIsIdempotent(t *testing.T) {
	bus := sqlite.NewEventBus(openDB(t, ":memory:"), 10*time.Millisecond)
	if err := bus.Subscribe(t.Context(), "sub", &recorder{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := bus.Subscribe(t.Context(), "late", &recorder{}); err == nil {
		t.Fatal("Subscribe after Close succeeded, want an error")
	}
}

// closingHandler records events and, on the event numbered at, signals
// reached and then blocks until its ctx is done, as a handler still busy
// with an event when Close lands would, before it returns nil.
type closingHandler struct {
	recorder
	at      int
	reached chan struct{}
	seen    int
}

func (h *closingHandler) Handle(ctx context.Context, e eventsourcing.Event) error {
	if err := h.recorder.Handle(ctx, e); err != nil {
		return err
	}
	h.seen++
	if h.seen == h.at {
		close(h.reached)
		<-ctx.Done()
	}
	return nil
}

// TestEventBus_CloseDuringHandlerStoresPosition covers Close landing while
// a handler is busy with an event (#181): the handler finishes, its
// position is still stored, the bus delivers nothing after it, and the
// ":memory:" database the bus shares survives the cancellation.
func TestEventBus_CloseDuringHandlerStoresPosition(t *testing.T) {
	db := openDB(t, ":memory:")
	store := sqlite.NewEventStore(db)
	appendEvents(t, store, "task-1", &taskCreated{ID: "1"}, &taskDone{ID: "1"}, &taskCreated{ID: "1"})

	bus := sqlite.NewEventBus(db, 10*time.Millisecond)
	h := &closingHandler{at: 2, reached: make(chan struct{})}
	if err := bus.Subscribe(t.Context(), "sub", h); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-h.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reached the second event")
	}
	bus.Close()

	var pos int64
	if err := db.QueryRowContext(t.Context(), "SELECT position FROM event_subscriptions WHERE name = 'sub'").Scan(&pos); err != nil {
		t.Fatalf("read position after Close: %v", err)
	}
	if pos != 2 {
		t.Fatalf("stored position after Close = %d, want 2: the event the handler finished", pos)
	}
	if n := h.count(); n != 2 {
		t.Fatalf("handler received %d events, want 2: none after Close", n)
	}
}
