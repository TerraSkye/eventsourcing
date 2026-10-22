// Command dashboard runs a small task service with two projections over an
// in-memory event store, and serves the projection dashboard for it. It
// appends a backlog of events at start and a steady trickle after, and the
// handlers are slowed down, so catching up and rebuilding take long enough
// to watch.
//
//	go run ./examples/dashboard
//
// then open http://localhost:8080 and press Rebuild on either projection.
// With -stall, task-list fails on one event for 20 seconds, to show a
// stalled projection and its recovery.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/dashboard"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

type TaskCreated struct {
	TaskID string
	Owner  string
}

func (e *TaskCreated) AggregateID() string { return e.TaskID }
func (e *TaskCreated) EventType() string   { return "demo.TaskCreated" }

type TaskCompleted struct{ TaskID string }

func (e *TaskCompleted) AggregateID() string { return e.TaskID }
func (e *TaskCompleted) EventType() string   { return "demo.TaskCompleted" }

func init() {
	eventsourcing.RegisterEvent(&TaskCreated{})
	eventsourcing.RegisterEvent(&TaskCompleted{})
}

var owners = []string{"ana", "bram", "chen", "dewi", "emre", "fleur", "gijs", "hana"}

func main() {
	addr := flag.String("addr", "localhost:8080", "address to serve the dashboard on")
	backlog := flag.Int("backlog", 60000, "events appended before the projections start")
	perSecond := flag.Int("rate", 40, "events appended per second after the backlog")
	speed := flag.Int("speed", 6000, "events per second a projection handles at most")
	stall := flag.Bool("stall", false, "make task-list fail on one event for 20 seconds")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	events := memory.NewEventStore(1)
	store := memory.NewProjectionStore()

	appendTasks(ctx, events, *backlog)
	log.Printf("appended a backlog of %d events", *backlog)

	list := newTaskList(*speed)
	if *stall {
		list.failAt = uint64(*backlog / 2)
	}
	byOwner := newOwnerCounts(*speed / 4)
	taskList := projection.NewRunner(list.definition(), events, store)
	tasksByOwner := projection.NewRunner(byOwner.definition(), events, store)
	go func() { _ = taskList.Run(ctx) }()
	go func() { _ = tasksByOwner.Run(ctx) }()
	go trickle(ctx, events, *perSecond)

	d := dashboard.New(store,
		dashboard.WithRunners(taskList, tasksByOwner),
		dashboard.WithReset(),
		dashboard.WithTitle("Task service"),
	)
	srv := &http.Server{Addr: *addr, Handler: d}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("dashboard on http://%s", *addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// appendTasks appends n events: tasks created and, for most, completed.
func appendTasks(ctx context.Context, events *memory.EventStore, n int) {
	for i := 0; i < n && ctx.Err() == nil; {
		id := uuid.NewString()
		envs := []eventsourcing.Envelope{
			{EventID: uuid.New(), StreamID: id, Version: 1, OccurredAt: time.Now(),
				Event: &TaskCreated{TaskID: id, Owner: owners[i%len(owners)]}},
		}
		if i%3 != 0 {
			envs = append(envs, eventsourcing.Envelope{EventID: uuid.New(), StreamID: id, Version: 2, OccurredAt: time.Now(),
				Event: &TaskCompleted{TaskID: id}})
		}
		if _, err := events.Save(ctx, envs, eventsourcing.NoStream{}); err != nil {
			log.Printf("append: %v", err)
			return
		}
		i += len(envs)
	}
}

// trickle keeps appending events, so the projections have something to do
// once they are live.
func trickle(ctx context.Context, events *memory.EventStore, perSecond int) {
	if perSecond <= 0 {
		return
	}
	t := time.NewTicker(time.Second / time.Duration(perSecond))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			appendTasks(ctx, events, 1)
		}
	}
}

// throttle slows a handler down to about perSecond events, so a demo can
// watch it catch up.
type throttle struct {
	mu    sync.Mutex
	every int
	seen  int
}

func newThrottle(perSecond int) *throttle {
	// time.Sleep rounds up to about a millisecond, so sleep once per
	// perSecond/1000 events.
	return &throttle{every: max(1, perSecond/1000)}
}

func (t *throttle) wait() {
	t.mu.Lock()
	t.seen++
	sleep := t.seen%t.every == 0
	t.mu.Unlock()
	if sleep {
		time.Sleep(time.Millisecond)
	}
}

// taskList is the open tasks: one unsegmented projection.
type taskList struct {
	*throttle
	mu   sync.Mutex
	open map[string]string // task ID to owner

	// failAt is a position the handler fails on for 20 seconds, from the
	// first time it sees it; 0 for never.
	failAt    uint64
	failUntil time.Time
}

func newTaskList(speed int) *taskList {
	return &taskList{throttle: newThrottle(speed), open: map[string]string{}}
}

func (l *taskList) definition() projection.Definition {
	return projection.Definition{
		Name:       "task-list",
		Handler:    eventsourcing.NewEventHandlerFunc(l.handle),
		EventTypes: []string{"demo.TaskCreated", "demo.TaskCompleted"},
		Reset:      l.reset,
		Owner:      "worker-1",
	}
}

func (l *taskList) handle(ctx context.Context, event eventsourcing.Event) error {
	l.wait()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failAt != 0 && eventsourcing.GlobalPositionFromContext(ctx) == l.failAt {
		if l.failUntil.IsZero() {
			l.failUntil = time.Now().Add(20 * time.Second)
		}
		if time.Now().Before(l.failUntil) {
			return errors.New("notify owner: dial tcp 10.0.3.7:443: connect: connection refused")
		}
	}
	switch e := event.(type) {
	case *TaskCreated:
		l.open[e.TaskID] = e.Owner
	case *TaskCompleted:
		delete(l.open, e.TaskID)
	}
	return nil
}

func (l *taskList) reset(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open = map[string]string{}
	return nil
}

// ownerCounts is how many tasks each owner created: a projection split into
// four segments by task, to show segments catching up side by side.
type ownerCounts struct {
	*throttle
	mu     sync.Mutex
	counts map[string]int
}

func newOwnerCounts(speed int) *ownerCounts {
	return &ownerCounts{throttle: newThrottle(speed), counts: map[string]int{}}
}

func (o *ownerCounts) definition() projection.Definition {
	return projection.Definition{
		Name:       "tasks-by-owner",
		Handler:    eventsourcing.NewEventHandlerFunc(o.handle),
		EventTypes: []string{"demo.TaskCreated"},
		Reset:      o.reset,
		Segments:   4,
		SegmentKey: projection.ByStreamID,
		Owner:      "worker-1",
	}
}

func (o *ownerCounts) handle(ctx context.Context, event eventsourcing.Event) error {
	o.wait()
	if e, ok := event.(*TaskCreated); ok {
		o.mu.Lock()
		o.counts[e.Owner]++
		o.mu.Unlock()
	}
	return nil
}

func (o *ownerCounts) reset(context.Context) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.counts = map[string]int{}
	return nil
}
