// Command dashboard runs a small task service with two projections, keeping
// its events, checkpoints and read models in one SQLite file, and serves
// the projection dashboard for it. It appends a backlog of events on first
// start and a steady trickle after, and the handlers are slowed down, so
// catching up and rebuilding take long enough to watch.
//
//	go run ./examples/dashboard
//
// then open http://localhost:8080 and press Rebuild on either projection.
// With -stall, task-list fails on one event for 20 seconds, to show a
// stalled projection and its recovery.
//
// Both projections run on -workers instances inside this process, as
// several processes of a service would run them, and share their segments.
// Every -churn the worker holding the most segments goes down, taking turns
// between a clean stop, which releases its segments to the others at once,
// and a crash, which leaves them silent until their claims expire after 15
// seconds. A stopped worker comes back after half of -churn, a crashed one
// once its claims have expired too.
//
// Workers can also run as processes of their own, on the same database:
//
//	go run ./examples/dashboard -workers 0               # dashboard and events
//	go run ./examples/dashboard -worker w1               # in a second terminal
//	go run ./examples/dashboard -worker w2               # in a third
//
// Ctrl+C on a worker stops it cleanly; kill -9 crashes it. -fresh starts
// over on an empty database.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/dashboard"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/sqlite"
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

// demoSchema is the read models' tables, next to the sqlite package's own.
const demoSchema = `
CREATE TABLE IF NOT EXISTS demo_open_tasks (
	task_id TEXT PRIMARY KEY,
	owner   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS demo_tasks_by_owner (
	owner TEXT PRIMARY KEY,
	tasks INTEGER NOT NULL
);`

func main() {
	addr := flag.String("addr", "localhost:8080", "address to serve the dashboard on")
	dbPath := flag.String("db", filepath.Join(os.TempDir(), "eventsourcing-dashboard-demo.db"), "SQLite database every process of the demo shares")
	fresh := flag.Bool("fresh", false, "delete the database first and start over")
	workerName := flag.String("worker", "", "run as a worker of this name only: no dashboard, no events appended")
	workers := flag.Int("workers", 3, "workers running the projections inside this process")
	churn := flag.Duration("churn", 20*time.Second, "how often a worker in this process goes down and comes back; 0 for never")
	backlog := flag.Int("backlog", 60000, "events appended on first start, before the projections start")
	perSecond := flag.Int("rate", 40, "events appended per second after the backlog")
	speed := flag.Int("speed", 6000, "events per second a worker handles at most")
	stall := flag.Bool("stall", false, "make task-list fail on one event for 20 seconds")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *fresh {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(*dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Fatal(err)
			}
		}
	}
	db, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	for _, schema := range []string{sqlite.Schema, demoSchema} {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			log.Fatalf("apply schema: %v", err)
		}
	}
	events := sqlite.NewEventStore(db)
	store := sqlite.NewProjectionStore(db)
	list := &taskList{throttle: newThrottle(*speed)}
	byOwner := &ownerCounts{throttle: newThrottle(*speed / 8)}

	if *workerName != "" {
		w := newWorker(*workerName, list, byOwner, events, store)
		w.start(ctx)
		log.Printf("%s running on %s", *workerName, *dbPath)
		<-ctx.Done()
		w.stop(false)
		return
	}

	if head, err := events.Head(ctx); err != nil {
		log.Fatal(err)
	} else if head == 0 {
		appendTasks(ctx, events, *backlog)
		log.Printf("appended a backlog of %d events", *backlog)
	}
	if *stall {
		head, _ := events.Head(ctx)
		list.failAt = head / 2
	}

	pool := make([]*worker, max(0, *workers))
	for i := range pool {
		pool[i] = newWorker("worker-"+strconv.Itoa(i+1), list, byOwner, events, store)
		pool[i].start(ctx)
	}
	go churnWorkers(ctx, store, byOwner.name(), pool, *churn)
	go trickle(ctx, events, *perSecond)

	d := dashboard.New(store,
		dashboard.WithProjections(list.name(), byOwner.name()),
		dashboard.WithReset(),
		dashboard.WithEvents(events),
		dashboard.WithTitle("Task service"),
	)
	srv := &http.Server{Addr: *addr, Handler: d}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf("dashboard on http://%s, database %s", *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	for _, w := range pool {
		w.stop(false)
	}
}

// appendTasks appends about n events: tasks created for a random owner
// and, two in three, completed. A task's events form one correlation
// chain, the completion caused by the creation, so the events view has
// chains to show.
func appendTasks(ctx context.Context, events eventsourcing.EventStore, n int) {
	for i := 0; i < n && ctx.Err() == nil; {
		id := uuid.NewString()
		created := uuid.New()
		envs := []eventsourcing.Envelope{
			{EventID: created, StreamID: id, Version: 1, OccurredAt: time.Now(), CorrelationID: created.String(),
				Event: &TaskCreated{TaskID: id, Owner: owners[rand.IntN(len(owners))]}},
		}
		if rand.IntN(3) != 0 {
			envs = append(envs, eventsourcing.Envelope{EventID: uuid.New(), StreamID: id, Version: 2, OccurredAt: time.Now(),
				CorrelationID: created.String(), CausationID: created.String(),
				Event: &TaskCompleted{TaskID: id}})
		}
		if _, err := events.Save(ctx, envs, eventsourcing.NoStream{}); err != nil {
			if ctx.Err() == nil {
				log.Printf("append: %v", err)
			}
			return
		}
		i += len(envs)
	}
}

// trickle keeps appending events, so the projections have something to do
// once they are live.
func trickle(ctx context.Context, events eventsourcing.EventStore, perSecond int) {
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

// taskList is the open tasks: one unsegmented projection, kept in the
// demo_open_tasks table.
type taskList struct {
	*throttle

	// failAt is a position the handler fails on for 20 seconds, from the
	// first time it sees it; 0 for never.
	failAt    uint64
	mu        sync.Mutex
	failUntil time.Time
}

func (l *taskList) name() string { return "task-list" }

func (l *taskList) definition(owner string) projection.Definition {
	return projection.Definition{
		Name:       l.name(),
		Handler:    eventsourcing.NewEventHandlerFunc(l.handle),
		EventTypes: []string{"demo.TaskCreated", "demo.TaskCompleted"},
		Reset:      l.reset,
		Owner:      owner,
	}
}

func (l *taskList) handle(ctx context.Context, event eventsourcing.Event) error {
	l.wait()
	if err := l.failing(eventsourcing.GlobalPositionFromContext(ctx)); err != nil {
		return err
	}
	tx := sqlite.Tx(ctx)
	var err error
	switch e := event.(type) {
	case *TaskCreated:
		_, err = tx.ExecContext(ctx, `INSERT INTO demo_open_tasks (task_id, owner) VALUES (?, ?)
			ON CONFLICT (task_id) DO NOTHING`, e.TaskID, e.Owner)
	case *TaskCompleted:
		_, err = tx.ExecContext(ctx, `DELETE FROM demo_open_tasks WHERE task_id = ?`, e.TaskID)
	}
	return err
}

// failing returns an error while the handler is to fail at position.
func (l *taskList) failing(position uint64) error {
	if l.failAt == 0 || position != l.failAt {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failUntil.IsZero() {
		l.failUntil = time.Now().Add(20 * time.Second)
	}
	if time.Now().Before(l.failUntil) {
		return errors.New("notify owner: dial tcp 10.0.3.7:443: connect: connection refused")
	}
	return nil
}

func (l *taskList) reset(ctx context.Context) error {
	_, err := sqlite.Tx(ctx).ExecContext(ctx, `DELETE FROM demo_open_tasks`)
	return err
}

// ownerCounts is how many tasks each owner created, kept in the
// demo_tasks_by_owner table: a projection split into eight segments by
// task, to show segments catching up side by side and moving between
// workers.
type ownerCounts struct {
	*throttle
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
	e, ok := event.(*TaskCreated)
	if !ok {
		return nil
	}
	_, err := sqlite.Tx(ctx).ExecContext(ctx, `INSERT INTO demo_tasks_by_owner (owner, tasks) VALUES (?, 1)
		ON CONFLICT (owner) DO UPDATE SET tasks = tasks + 1`, e.Owner)
	return err
}

func (o *ownerCounts) reset(ctx context.Context) error {
	_, err := sqlite.Tx(ctx).ExecContext(ctx, `DELETE FROM demo_tasks_by_owner`)
	return err
}

// worker is one instance of the service's projections, as one process of
// it would run them. It can be stopped cleanly, crashed and started again.
type worker struct {
	name    string
	list    *taskList
	byOwner *ownerCounts
	events  eventsourcing.EventStore
	store   *crashableStore

	cancel context.CancelFunc
	done   sync.WaitGroup
}

func newWorker(name string, list *taskList, byOwner *ownerCounts, events eventsourcing.EventStore, store projection.Store) *worker {
	return &worker{name: name, list: list, byOwner: byOwner, events: events, store: &crashableStore{Store: store}}
}

// start runs new runners of both projections for w until ctx ends or w
// goes down.
func (w *worker) start(ctx context.Context) {
	w.store.crashed.Store(false)
	ctx, w.cancel = context.WithCancel(ctx)
	reader := w.events.(eventsourcing.GlobalReader)
	for _, def := range []projection.Definition{w.list.definition(w.name), w.byOwner.definition(w.name)} {
		r := projection.NewRunner(def, reader, w.store)
		w.done.Go(func() { _ = r.Run(ctx) })
	}
}

// stop stops w's runners, which release their segments on the way out
// unless crash is set: then they stay claimed by w until their claims
// expire.
func (w *worker) stop(crash bool) {
	w.store.crashed.Store(crash)
	w.cancel()
	w.done.Wait()
}

// churnWorkers takes a worker down every interval, the one holding the most
// segments, alternating clean stops and crashes. A stopped worker comes
// back after half the interval. A crashed one stays down until its claims
// have expired as well, so the page shows another worker taking its
// segments over rather than it reclaiming them itself.
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
			// A runner appends a random part to the worker's name.
			if i := strings.LastIndexByte(st.Owner, '/'); i >= 0 {
				held[st.Owner[:i]]++
			}
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
// worker has crashed, its runners' Release calls and the heartbeat they
// leave the instances with are lost, as a process that died would never
// send them: its claims and its instance rows stay until they expire.
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

func (s *crashableStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Instance, error) {
	if ttl == 0 && s.crashed.Load() {
		return nil, nil
	}
	return s.Store.Heartbeat(ctx, name, owner, version, ttl)
}
