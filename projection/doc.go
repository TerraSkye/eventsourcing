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
//	r := projection.NewRunner(projection.Projection{
//		Name:     "task-list",
//		Handlers: projector.EventHandlers(),
//	}, eventStore, projection.NewMemoryStore())
//
//	go r.Run(ctx)
//
// For an in-memory read model, [NewMemoryStore] is enough; the whole
// projection is replayed on every start, which is right for a read model
// that is empty after a restart anyway. For a durable read model, use the
// Store that belongs to your database — for example
// projection/postgres.NewStore — so the handlers' writes and the
// checkpoint move land in the same transaction.
//
// # Writing a projector
//
// A projector is an ordinary struct with one method per event type it
// handles, grouped with [eventsourcing.NewEventGroupProcessor]:
//
//	type Projector struct {
//		mu    sync.RWMutex
//		tasks map[string]*Task
//	}
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		p.mu.Lock()
//		defer p.mu.Unlock()
//		p.tasks[e.TaskID.String()] = &Task{ID: e.TaskID.String(), Title: e.Title}
//		return nil
//	}
//
//	func (p *Projector) EventHandlers() *eventsourcing.EventGroupProcessor {
//		return eventsourcing.NewEventGroupProcessor(
//			eventsourcing.OnEvent(p.OnTaskCreated),
//		)
//	}
//
// With a Postgres store, handlers reach the batch's transaction through
// its own accessor:
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		_, err := postgres.Tx(ctx).Exec(ctx,
//			`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//			e.TaskID, e.Title)
//		return err
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
//   - Bump [Projection.Version] when you change how the projector writes
//     its read model. On start, a runner whose stored version differs from
//     its configured one rebuilds automatically.
//   - Call [Runner.Rebuild] on the active instance.
//
// # Failures
//
// A runner never gives up. When a batch fails, it reports [Stalled] with
// the error, backs off, and retries from its last committed checkpoint. By
// default a failing event stops the projection at that event until the
// cause is fixed. [Projection.OnError] can make the runner skip failing
// events instead, recording them through [DeadLetterStore] when the Store
// supports it.
//
// A handler that returns [eventsourcing.SkippedEventError] has declined
// the event; that is not a failure, and the checkpoint moves past it.
//
// # Waking up
//
// At the head of the log, a runner waits until one of the following
// happens, then reads again: the store signals an append (if it
// implements [eventsourcing.Notifier]), something calls [Runner.Nudge]
// (for example [NudgeOnSave] after a command saved events), or the poll
// interval elapses. Nudges and notifications only reduce latency;
// correctness never depends on them, because the poll always runs.
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
package projection
