// Package projection keeps read models up to date from the global event log.
//
// A projection turns events into state that is cheap to query: a table of
// orders to ship, a list of tasks, a search index. This package provides
// [Runner], which reads events from an [eventsourcing.GlobalReader], routes
// each one to your handlers, and remembers how far it got, so that a
// projection survives restarts, catches up on history, and can be rebuilt
// from scratch.
//
// # Writing a projector
//
// A projector is an ordinary struct with one method per event type it
// handles, grouped with [eventsourcing.NewEventGroupProcessor]. Nothing in
// it is specific to this package:
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
//	func (p *Projector) OnTaskCompleted(ctx context.Context, e *events.TaskCompleted) error {
//		p.mu.Lock()
//		defer p.mu.Unlock()
//		if t, ok := p.tasks[e.TaskID.String()]; ok {
//			t.Completed = true
//		}
//		return nil
//	}
//
//	func (p *Projector) EventHandlers() *eventsourcing.EventGroupProcessor {
//		return eventsourcing.NewEventGroupProcessor(
//			eventsourcing.OnEvent(p.OnTaskCreated),
//			eventsourcing.OnEvent(p.OnTaskCompleted),
//		)
//	}
//
// Running it takes a name, the handlers and the store:
//
//	projector := tasklist.NewProjector()
//
//	taskList := projection.NewRunner("task-list", projector.EventHandlers(), store)
//	go taskList.Run(ctx)
//
// Without further options the runner keeps its checkpoint in memory, so it
// replays the whole log on every start. That is exactly right for an
// in-memory read model, which is empty after a restart anyway.
//
// # Durable read models
//
// For a read model in a database, give the runner a [CheckpointStore] for
// that same database. The runner then opens one unit of work per batch of
// events (a transaction, for SQL databases), runs your handlers inside it,
// and stores the new checkpoint in it before committing. Either the read
// model changes and the checkpoint move together, or neither does, so each
// event is applied exactly once even across crashes.
//
// Handlers reach the unit of work through the context. With the Postgres
// checkpoint store from the projection/postgres package:
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		_, err := postgres.Tx(ctx).Exec(ctx,
//			`INSERT INTO tasks (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
//			e.TaskID, e.Title)
//		return err
//	}
//
//	func (p *Projector) Reset(ctx context.Context) error {
//		_, err := postgres.Tx(ctx).Exec(ctx, `TRUNCATE tasks`)
//		return err
//	}
//
//	taskList := projection.NewRunner("task-list", projector.EventHandlers(), store,
//		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
//		projection.WithReset(projector.Reset),
//		projection.WithVersion(1),
//	)
//
// A checkpoint store that cannot roll back, such as the in-memory default,
// gives at-least-once delivery instead: after a failure, the events of the
// failed batch are applied again. Handlers used with such a store must be
// safe to repeat.
//
// Handlers receive the usual envelope context, so
// [eventsourcing.GlobalVersionFromContext], [eventsourcing.StreamIDFromContext]
// and the other *FromContext helpers work inside them.
//
// # Catching up and live
//
// A runner that is behind the head of the log is catching up; once a read
// reaches the head it is live. [Runner.Status] reports the current [Phase],
// and [Runner.Live] answers the common question directly. Use it to gate
// work that must not run on a half-built read model:
//
//   - an automation that processes a todo list (see the automation package),
//   - a readiness probe, so traffic is not routed to an instance whose read
//     models are still rebuilding,
//   - tests that wait for a projection with [Runner.WaitUntil].
//
// Live is a snapshot: it means the last read reached the head, not that
// nothing was appended since. Code that needs a guarantee about one specific
// event should wait for its position with [Runner.WaitUntil] instead.
//
// # Replaying
//
// Catching up and replaying are different things. A projection catches up
// whenever it is behind, for example after an import. It replays when it
// processes events it already processed before, which happens after a
// rebuild, or on its first run over existing history. Handlers ask
// [IsReplaying] to skip work that only makes sense for new events, such as
// pushing an update to connected browsers:
//
//	if !projection.IsReplaying(ctx) {
//		p.updates.Publish(e.TaskID)
//	}
//
// # Rebuilding
//
// A projection is rebuilt by wiping its read model and processing the log
// again from the start. [WithReset] registers how to wipe it; the reset runs
// in the same unit of work that moves the checkpoint back to the start, so
// a rebuild never leaves a half-wiped read model with a stale checkpoint.
//
// There are two ways to trigger a rebuild:
//
//   - Bump [WithVersion] when you change how the projector writes its read
//     model. On start, a runner whose stored version differs from its
//     configured one rebuilds automatically. This is the reliable way to
//     rebuild across several instances.
//   - Call [Runner.Rebuild] on the active instance, for example from an
//     admin endpoint or to recover from a projector bug.
//
// # Failures
//
// A runner never gives up. When a batch fails, it reports [Stalled] with
// the error, backs off, and retries from its last committed checkpoint. By
// default a failing event stops the projection at that event until the
// cause is fixed; a silently skipped event would leave the read model wrong
// with nothing to show for it. [WithErrorPolicy] can make the runner skip
// failing events instead, recording them as dead letters when the
// checkpoint store supports it (see [DeadLetterStore]).
//
// A handler that returns [eventsourcing.SkippedEventError] has declined the
// event; that is not a failure, and the checkpoint moves past it.
//
// # Waking up
//
// At the head of the log, a runner waits until one of the following
// happens, then reads again:
//
//   - the store signals an append, if it implements
//     [eventsourcing.Notifier];
//   - something calls [Runner.Nudge], for example the [NudgeOnSave] store
//     middleware after a command saved events;
//   - its poll interval elapses ([WithPollInterval]).
//
// Nudges and notifications only reduce latency. Correctness never depends
// on them, because the poll always runs.
//
// # Several instances
//
// Only one instance may run a given projection at a time, or two of them
// would apply the same events. When the checkpoint store implements
// [Locker], every runner competes for a lock per projection name: one
// becomes active, the others report [Standby] and take over when the
// active one goes away. When it does not, run each projection in a single
// instance.
//
// When the checkpoint store implements [StatusStore], the active runner
// publishes its [Status] there, so other processes can follow it; see
// [Remote].
package projection
