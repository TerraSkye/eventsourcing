// Package projection keeps read models up to date from the global event log.
//
// A projection turns events into state that is cheap to query: a table of
// orders to ship, a list of tasks, a search index. This package provides
// [Runner], which reads events from an [eventsourcing.GlobalReader], routes
// each one to your handlers, and remembers how far it got so that a
// projection survives restarts, catches up on history, and can be rebuilt
// from scratch.
//
// # Three concepts
//
// A user wires up three things:
//
//  1. A [Projection] value, which describes the projection — its name, its
//     handlers, and anything optional like Version/Reset or segmentation.
//  2. An [eventsourcing.GlobalReader], which is the event store the
//     projection reads from.
//  3. A [Store], which persists everything the projection needs to survive
//     restarts and share work between instances: checkpoints, segment
//     claims, and the per-segment Status that other instances follow.
//     One Store lives in the same database as the read models.
//
// That's the whole surface:
//
//	list := tasklist.NewTaskList()
//	r := projection.NewRunner(tasklist.NewProjection(list), eventStore, memory.NewStore())
//
//	go r.Run(ctx)
//
// For an in-memory read model, projection/memory.NewStore is enough; the
// whole projection is replayed on every start, which is right for a read
// model that is empty after a restart anyway. For a durable read model,
// use the Store that belongs to your database — for example
// projection/postgres.NewStore — so the handlers' writes and the
// checkpoint move land in the same transaction.
//
// # Writing a projection
//
// A slice that owns a read model has three parts: the read model itself,
// the projection that writes it, and the query that reads it. The
// projection is a constructor that returns a [Projection]: one handler
// that switches on the event type, and the registered names of the types
// it handles. The query reads the read model directly; it never needs the
// projection.
//
// An in-memory read model is a type that both sides share:
//
//	// TaskList is the read model: written by the projection, read by the
//	// query.
//	type TaskList struct {
//		mu    sync.RWMutex
//		tasks map[string]*Task
//	}
//
//	func NewProjection(list *TaskList) projection.Projection {
//		return projection.Projection{
//			Name:    "task-list",
//			Handler: eventsourcing.NewEventHandlerFunc(list.handle),
//			Events:  []string{"TaskCreated"},
//		}
//	}
//
//	func (l *TaskList) handle(ctx context.Context, event eventsourcing.Event) error {
//		l.mu.Lock()
//		defer l.mu.Unlock()
//		switch e := event.(type) {
//		case *events.TaskCreated:
//			l.tasks[e.TaskID.String()] = &Task{ID: e.TaskID.String(), Title: e.Title}
//		}
//		return nil
//	}
//
// A handler that prefers one method per event type can group them with
// [eventsourcing.NewEventGroupProcessor], pass the group as Handler, and
// pass the group's StreamFilter as Events.
//
// A read model in Postgres is its tables, so the projection keeps no state
// at all. Handlers write through the batch's transaction, and the query
// reads the tables with its own connection:
//
//	func NewProjection() projection.Projection {
//		return projection.Projection{
//			Name:    "task-list",
//			Handler: eventsourcing.NewEventHandlerFunc(handle),
//			Events:  []string{"TaskCreated"},
//		}
//	}
//
//	func handle(ctx context.Context, event eventsourcing.Event) error {
//		switch e := event.(type) {
//		case *events.TaskCreated:
//			_, err := postgres.Tx(ctx).Exec(ctx,
//				`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//				e.TaskID, e.Title)
//			return err
//		}
//		return nil
//	}
//
// # Catching up and live
//
// A runner that is behind the head of the log is [CatchingUp]; once a read
// reaches the head it is [Live]. [Runner.Status] reports the current
// [Phase], and [Runner.Live] answers the common question directly. Use it
// to gate work that must not run on a half-built read model: a readiness
// probe, an automation, or a test's wait with [Runner.WaitUntil].
//
// # Replaying
//
// Catching up and replaying are different things. A projection catches up
// whenever it is behind, for example after an import. It replays when it
// processes events it already processed before, which happens after a
// rebuild, or on its first run over existing history. Handlers ask
// [IsReplaying] to skip work that only makes sense for new events, such as
// pushing an update to connected browsers.
//
// # Rebuilding
//
// A projection is rebuilt by wiping its read model and processing the log
// again from the start. [Projection.Reset] says how to wipe it; the reset
// runs in the same unit of work that moves the checkpoint back to zero.
//
// There are two ways to trigger a rebuild:
//
//   - Bump [Projection.Version] when you change how the projection writes
//     its read model. On start, a runner whose stored version differs from
//     its configured one rebuilds automatically. A segmented projection
//     has no Version: rebuild it under a new [Projection.Name].
//   - Call [Runner.Rebuild] on the active instance.
//
// Adding an event type to [Projection.Events] counts as a change: without
// a Version bump, events of the new type stored before the change are
// never applied.
//
// A rebuild in place empties the read model until the runner has caught
// up. When that takes too long, build the new version under a new
// [Projection.Name], writing to new tables, and switch the queries over
// once it is [Live].
//
// # Failures
//
// A runner never gives up. When a batch fails, it reports [Stalled] with
// the error, backs off (from 0.5s up to 30s, jittered), and retries from
// its last committed checkpoint. By
// default a failing event stops the projection at that event until the
// cause is fixed. [Projection.OnError] can make the runner skip failing
// events instead, recording them through [DeadLetterStore] when the Store
// supports it. A skipped event does not hold back its stream: later events
// of the same stream are still applied.
//
// Exactly-once covers only what a handler writes through the Store's unit
// of work, such as postgres.Tx(ctx). Anything else a handler does, such as
// sending an email, calling a webhook or pushing to browsers, happens at
// least once: a retried batch does it again, and [IsReplaying] does not
// prevent that, since a retry is not a replay. Side effects that must not
// repeat belong in an automation, not in a projection.
//
// The runner does not log. To log handler failures, wrap the Handler, for
// example with logging.WithEventLogging; [Runner.Status] reports why a
// segment is stalled.
//
// A handler that returns [eventsourcing.SkippedEventError] has declined
// the event; that is not a failure, and the checkpoint moves past it.
//
// # Waking up
//
// At the head of the log, a runner waits until one of the following
// happens, then reads again: something calls [Runner.Nudge], or the poll
// interval elapses. Nudges only reduce latency; correctness never depends
// on them, because the poll always runs.
//
// # Several instances
//
// Only one instance may run a given projection (or segment of one) at a
// time, or two of them would apply the same events. Every runner competes
// for each segment's claim through [Store.Claim]; the one that holds the
// claim becomes active, the others report [Standby] and keep trying until
// the active one goes away or its claim expires.
//
// # Scaling out
//
// A projection whose one instance cannot keep up is segmented by setting
// [Projection.Segments] and [Projection.SegmentKey]. Each event goes to
// the segment its key maps to; segments spread across every instance of
// the program through the same claim mechanism as a single-segment
// projection, each with its own checkpoint and status. Handlers are the
// same as for an unsegmented projection, as long as no two segments write
// the same read-model row.
//
// Segmenting has two costs. Each segment reads the log on its own, so a
// projection with N segments reads it N times. And a read model that
// combines events from many streams into one row, such as a total or a
// count, cannot be segmented at all, since no key keeps that row in one
// segment.
package projection
