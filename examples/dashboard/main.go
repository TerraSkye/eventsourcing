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
//
// tasks-by-owner runs on -workers instances, as several processes of a
// service would, and they share its segments. Every -churn one of them goes
// down, taking turns between a clean stop, which releases its segments to
// the others at once, and a crash, which leaves them silent until their
// claims expire after 15 seconds. A stopped worker comes back after half
// of -churn, a crashed one once its claims have expired too.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"math/rand/v2"
	"net/http"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
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
	workers := flag.Int("workers", 3, "instances running tasks-by-owner, sharing its segments")
	churn := flag.Duration("churn", 20*time.Second, "how often a worker goes down and comes back; 0 for never")
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
	byOwner := newOwnerCounts(*speed / 8)
	taskList := projection.NewRunner(list.definition(), events, store)
	go func() { _ = taskList.Run(ctx) }()
	pool := make([]*worker, max(1, *workers))
	for i := range pool {
		pool[i] = newWorker("worker-"+strconv.Itoa(i+1), byOwner, events, store)
		pool[i].start(ctx)
	}
	go churnWorkers(ctx, store, byOwner.name(), pool, *churn)
	go trickle(ctx, events, *perSecond)

	d := dashboard.New(store,
		dashboard.WithRunners(taskList),
		dashboard.WithProjections(byOwner.name()),
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
// eight segments by task, to show segments catching up side by side and
// moving between workers. Its counts are shared by every worker, as a
// database table would be.
type ownerCounts struct {
	*throttle
	mu     sync.Mutex
	counts map[string]int
}

func newOwnerCounts(speed int) *ownerCounts {
	return &ownerCounts{throttle: newThrottle(speed), counts: map[string]int{}}
}

func (o *ownerCounts) name() string { return "tasks-by-owner" }

func (o *ownerCounts) definition(owner string) projection.Definition {
	return projection.Definition{
		Name:       o.name(),
		Handler:    eventsourcing.NewEventHandlerFunc(o.handle),
		EventTypes: []string{"demo.TaskCreated"},
		Reset:      o.reset,
		Segments:   8,
		SegmentKey: projection.ByStreamID,
		Owner:      owner,
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

// worker is one instance of tasks-by-owner, as one process of the service
// would run it. It can be stopped cleanly, crashed and started again.
type worker struct {
	name    string
	byOwner *ownerCounts
	events  *memory.EventStore
	store   *crashableStore

	cancel context.CancelFunc
	done   chan struct{}
}

func newWorker(name string, byOwner *ownerCounts, events *memory.EventStore, store projection.Store) *worker {
	return &worker{name: name, byOwner: byOwner, events: events, store: &crashableStore{Store: store}}
}

// start runs a new runner for w until ctx ends or w goes down.
func (w *worker) start(ctx context.Context) {
	w.store.crashed.Store(false)
	r := projection.NewRunner(w.byOwner.definition(w.name), w.events, w.store)
	ctx, w.cancel = context.WithCancel(ctx)
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		_ = r.Run(ctx)
	}()
}

// stop stops w's runner, which releases its segments on the way out unless
// crash is set: then they stay claimed by w until their claims expire.
func (w *worker) stop(crash bool) {
	w.store.crashed.Store(crash)
	w.cancel()
	<-w.done
}

// churnWorkers takes a worker down every interval, the one holding the most
// segments, alternating clean stops and crashes. A stopped worker comes back after
// half the interval. A crashed one stays down until its claims have
// expired as well, so the page shows another worker taking its segments
// over rather than it reclaiming them itself.
func churnWorkers(ctx context.Context, store projection.Store, name string, pool []*worker, interval time.Duration) {
	if interval <= 0 || len(pool) < 2 {
		return
	}
	crash := false
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		t.Stop()
		w := busiest(ctx, store, name, pool)
		if crash {
			log.Printf("%s crashed: its segments stay claimed until the claims expire", w.name)
		} else {
			log.Printf("%s stopped: it released its segments", w.name)
		}
		w.stop(crash)
		down := interval / 2
		if crash {
			down += projection.DefaultClaimTTL
		}
		crash = !crash
		select {
		case <-ctx.Done():
			return
		case <-time.After(down):
		}
		log.Printf("%s started", w.name)
		w.start(ctx)
		t.Reset(interval)
	}
}

// busiest returns the worker holding the most segments of the projection,
// so taking it down moves segments the page shows, or a random one when
// none holds any.
func busiest(ctx context.Context, store projection.Store, name string, pool []*worker) *worker {
	held := map[string]int{}
	if statuses, err := store.LoadStatuses(ctx, name); err == nil {
		for _, st := range statuses {
			held[st.Owner]++
		}
	}
	w := pool[rand.IntN(len(pool))]
	for _, c := range pool {
		if held[c.name] > held[w.name] {
			w = c
		}
	}
	return w
}

// crashableStore is a worker's view of the projection store. Once the
// worker has crashed, its runner's Release is lost, as a process that died
// would never send it.
type crashableStore struct {
	projection.Store
	crashed atomic.Bool
}

func (s *crashableStore) Release(ctx context.Context, l projection.Lease) error {
	if s.crashed.Load() {
		return nil
	}
	return s.Store.Release(ctx, l)
}
