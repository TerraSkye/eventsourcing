package projection

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/terraskye/eventsourcing"
)

// errNotImplemented marks the parts of this package that are still a
// design draft: signatures and documentation only.
var errNotImplemented = errors.New("projection: not implemented yet")

var (
	// ErrNotActive is returned by [Runner.Rebuild] when this instance is not
	// the one running the projection, either because [Runner.Run] has not
	// been called or because another instance holds the projection's lock.
	ErrNotActive = errors.New("projection: runner is not active on this instance")

	// ErrNoReset is returned by [Runner.Rebuild] when the runner was created
	// without [WithReset], so it has no way to wipe its read model.
	ErrNoReset = errors.New("projection: rebuild requires WithReset")
)

// Router routes events to a projector's handlers and knows which event
// types it handles.
//
// [*eventsourcing.EventGroupProcessor] satisfies Router, so the value a
// projector's EventHandlers method returns can be passed to [NewRunner]
// as is.
type Router interface {
	eventsourcing.EventHandler

	// StreamFilter returns the registered names of every event type the
	// router handles. The runner reads only these event types.
	StreamFilter() []string
}

// Runner keeps one projection up to date: it reads the global log after
// its checkpoint, routes each event through the projection's handlers, and
// commits the handlers' writes together with the new checkpoint.
//
// Create a Runner with [NewRunner] and start it with [Runner.Run]. All
// other methods are safe to call from any goroutine, before, during or
// after Run.
//
// Runner implements [Dependency], so it can gate an automation directly.
type Runner struct {
	name     string
	handlers eventsourcing.EventHandler
	store    eventsourcing.GlobalReader
	cfg      config

	nudge  chan struct{}
	status atomic.Pointer[Status]
}

// NewRunner creates a Runner for the projection called name, which routes
// events from store through handlers.
//
// name identifies the projection. It keys the checkpoint, the stored
// status and the lock, so it must be unique per read model and stable
// across deploys; renaming a projection makes it start from scratch.
//
// The runner reads only the event types handlers reports through
// StreamFilter. Every event type a handler covers must therefore be
// registered with [eventsourcing.RegisterEvent]: an unregistered type is
// left out of StreamFilter, and its events would never be read.
//
// Without options, the runner keeps its checkpoint in memory, reads up to
// 500 events per batch, and polls for new events every second. See the
// With* functions for the alternatives.
//
// NewRunner panics on wiring mistakes that would otherwise fail silently:
// an empty name, nil handlers or store, a StreamFilter that returns no
// event types, or [WithVersion] without [WithReset].
func NewRunner(name string, handlers Router, store eventsourcing.GlobalReader, opts ...Option) *Runner {
	switch {
	case name == "":
		panic("projection: NewRunner requires a name")
	case handlers == nil:
		panic(fmt.Sprintf("projection %s: NewRunner requires handlers", name))
	case store == nil:
		panic(fmt.Sprintf("projection %s: NewRunner requires a store", name))
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	if cfg.version != 0 && cfg.reset == nil {
		panic(fmt.Sprintf("projection %s: WithVersion requires WithReset", name))
	}

	filter := handlers.StreamFilter()
	if len(filter) == 0 {
		panic(fmt.Sprintf("projection %s: handlers cover no registered event types; register them with eventsourcing.RegisterEvent", name))
	}
	cfg.read.EventTypes = filter

	var h eventsourcing.EventHandler = handlers
	for i := len(cfg.middleware) - 1; i >= 0; i-- {
		h = cfg.middleware[i](h)
	}

	r := &Runner{
		name:     name,
		handlers: h,
		store:    store,
		cfg:      cfg,
		nudge:    make(chan struct{}, 1),
	}
	r.status.Store(&Status{Phase: Starting, Version: cfg.version})
	return r
}

// Name returns the projection's name, as passed to [NewRunner].
func (r *Runner) Name() string { return r.name }

// Run processes events until ctx is done, and then returns ctx.Err().
//
// Run never returns because of a failure. When a batch fails, the runner
// reports [Stalled], backs off (see [WithRetryStrategy]), and retries from
// its last committed checkpoint. A [Runner.Rebuild] requested while stalled
// is carried out, which is how a projection blocked by a projector bug is
// recovered after the fix is deployed.
//
// On start, Run loads the checkpoint. If none exists, the projection starts
// from the beginning of the log. If the stored version differs from the
// one set with [WithVersion], the projection is rebuilt first.
//
// When the checkpoint store implements [Locker], Run first competes for the
// projection's lock and reports [Standby] until it holds it. If the lock is
// lost later, Run stops processing at the next batch boundary and competes
// again.
//
// Call Run once per Runner, typically in its own goroutine.
func (r *Runner) Run(ctx context.Context) error {
	panic(errNotImplemented)
}

// Status returns the runner's current status as seen by this instance.
//
// On an instance in [Standby], the status describes the waiting runner
// itself; use [Runner.Live] or [Remote] to learn about the projection as
// run by the active instance.
func (r *Runner) Status() Status {
	return *r.status.Load()
}

// Live reports whether the projection is live: caught up with the head of
// the log, on whichever instance runs it.
//
// On the active instance, Live answers from memory and never fails. On an
// instance in [Standby], it reads the status the active instance published
// to the checkpoint store, which requires a [StatusStore]; without one,
// it reports false.
//
// Live implements [Dependency].
func (r *Runner) Live(ctx context.Context) (bool, error) {
	panic(errNotImplemented)
}

// Rebuild wipes the read model and processes the log again from the start.
//
// The reset registered with [WithReset] and the move of the checkpoint back
// to the start are committed in one unit of work, between two batches, so
// a rebuild never races with a batch in progress. Rebuild returns once that
// unit of work is committed; follow [Runner.Status] to see the projection
// go from [CatchingUp] to [Live] again. Events processed during the rebuild
// up to the old checkpoint are reported as replayed (see [IsReplaying]).
//
// Rebuild only reaches the runner in this process. It returns [ErrNotActive]
// when this instance is not running the projection, and [ErrNoReset] when
// the runner has no reset function. To rebuild across instances, bump
// [WithVersion] instead.
func (r *Runner) Rebuild(ctx context.Context) error {
	panic(errNotImplemented)
}

// WaitUntil blocks until the projection has processed the log up to and
// including position, or until ctx is done.
//
// position is a [eventsourcing.Envelope.GlobalVersion], typically
// [eventsourcing.AppendResult.GlobalVersion] from the command whose
// effects the caller wants to see, which gives read-your-own-writes:
//
//	res, err := handler(ctx, cmd)
//	if err != nil {
//		return err
//	}
//	if err := taskList.WaitUntil(ctx, res.GlobalVersion); err != nil {
//		return err // ctx expired before the projection got there
//	}
//
// Events the projection does not handle count as processed, so WaitUntil
// returns as soon as the checkpoint reaches position, whether or not the
// event at position was relevant to it. On an instance in [Standby],
// WaitUntil follows the status the active instance publishes, which
// requires a [StatusStore].
//
// WaitUntil is also the way to wait for a projection in tests:
// append events, then wait for the position of the last one.
func (r *Runner) WaitUntil(ctx context.Context, position uint64) error {
	panic(errNotImplemented)
}

// Nudge asks the runner to read now instead of waiting for its next poll.
//
// Nudge never blocks, and nudges coalesce: a burst of nudges while the
// runner is busy results in a single extra read once it is done, which
// picks up everything appended in the meantime. A nudge that arrives while
// the runner is stalled is kept until it recovers; it does not cut the
// backoff short.
//
// Nudge only reaches the runner in this process. Other instances rely on
// the store's [eventsourcing.Notifier] or on polling.
func (r *Runner) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default: // a nudge is already pending
	}
}

// Watch registers fn to be called after every committed batch that
// contained events, and on every change of [Phase]. It is how an
// automation waiting on this projection learns, without polling, that
// there may be new work or that the projection became live.
//
// fn runs on the runner's goroutine and must not block; typically it is
// another component's Nudge method. Watch implements [Watcher].
func (r *Runner) Watch(fn func()) {
	panic(errNotImplemented)
}

var (
	_ Dependency = (*Runner)(nil)
	_ Watcher    = (*Runner)(nil)
	_ Nudger     = (*Runner)(nil)
)
