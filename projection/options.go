package projection

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
)

// Option configures a [Runner]; pass options to [NewRunner].
type Option func(*config)

type config struct {
	checkpoints  CheckpointStore
	reset        func(ctx context.Context) error
	version      int
	read         eventsourcing.ReadOptions
	pollInterval time.Duration
	middleware   []eventsourcing.EventHandlerMiddleware
	retry        backoff.BackOff
	heartbeat    time.Duration
	errorPolicy  ErrorPolicy
	onCatchingUp func(ctx context.Context, s Status) error
	onLive       func(ctx context.Context, s Status) error
	newBuffer    func() Flusher
}

func defaultConfig() config {
	return config{
		read:         eventsourcing.ReadOptions{Limit: 500},
		pollInterval: time.Second,
		retry:        backoff.NewExponentialBackOff(backoff.WithMaxElapsedTime(0)),
		errorPolicy:  StallOnError,
	}
}

// WithCheckpoints sets where the runner stores its checkpoint.
//
// Use the checkpoint store that belongs to the database holding the read
// model, so that the read model's changes and the checkpoint commit
// together; see projection/postgres for Postgres.
//
// The default keeps the checkpoint in memory, so the projection is rebuilt
// from the start of the log on every start. That suits in-memory read
// models and tests. It cannot roll back a failed batch, so handlers used
// with it must be safe to apply twice.
//
// The runner looks for the optional [StatusStore], [Locker] and
// [DeadLetterStore] interfaces on the store passed here. A wrapper around a
// checkpoint store (for telemetry, say) hides them unless it implements
// them too.
func WithCheckpoints(store CheckpointStore) Option {
	return func(c *config) { c.checkpoints = store }
}

// WithReset registers how to wipe the read model, which enables rebuilds
// through [Runner.Rebuild] and [WithVersion].
//
// fn must remove everything the projection's handlers wrote. It runs in the
// same unit of work as the move of the checkpoint back to the start, and
// reaches that unit of work through ctx the same way handlers do, so a
// failed reset changes nothing:
//
//	func (p *Projector) Reset(ctx context.Context) error {
//		_, err := postgres.Tx(ctx).Exec(ctx, `TRUNCATE tasks`)
//		return err
//	}
func WithReset(fn func(ctx context.Context) error) Option {
	return func(c *config) { c.reset = fn }
}

// WithVersion sets the version of the projector's code, which is stored
// with the checkpoint. When a runner starts and finds a checkpoint written
// by a different version, it rebuilds the projection before processing new
// events.
//
// Bump the version whenever the projector starts writing its read model
// differently: a new column, a fixed bug in how an event is applied, a
// newly handled event type that affects existing rows. Deploying the new
// version is then all it takes to rebuild, on whichever instance runs the
// projection.
//
// The default version is 0. A projection that has never run starts at the
// configured version without a rebuild, since there is nothing to wipe.
// WithVersion requires [WithReset].
func WithVersion(v int) Option {
	return func(c *config) { c.version = v }
}

// WithBatchSize sets the maximum number of events read and committed in one
// unit of work. The default is 500.
//
// While catching up, larger batches mean fewer transactions and checkpoint
// writes, which usually dominate the cost of a rebuild. While live, batches
// are small anyway, because they hold only what was appended since the
// last read. A failing batch is retried as a whole, so very large batches
// make each retry more expensive.
func WithBatchSize(n int) Option {
	return func(c *config) { c.read.Limit = n }
}

// WithStreamPrefixes restricts the projection to events in streams whose ID
// starts with one of prefixes, on top of the event-type filter derived from
// the handlers.
func WithStreamPrefixes(prefixes ...string) Option {
	return func(c *config) { c.read.StreamPrefixes = prefixes }
}

// WithPollInterval sets how long a live runner waits before reading again
// when nothing woke it earlier. The default is one second.
//
// The poll is the safety net under [eventsourcing.Notifier] and
// [Runner.Nudge]: it guarantees new events are picked up even when no
// notification arrives. With a store that does not notify, it also bounds
// how stale the read model can get.
func WithPollInterval(d time.Duration) Option {
	return func(c *config) { c.pollInterval = d }
}

// WithMiddleware wraps the handlers in mw, in the same order as
// [eventsourcing.EventBus.Use]: the first middleware is the outermost and
// runs first for each event.
//
// Middleware is applied by the runner rather than by the caller, so that
// wrapping the handlers does not hide their StreamFilter method.
func WithMiddleware(mw ...eventsourcing.EventHandlerMiddleware) Option {
	return func(c *config) { c.middleware = append(c.middleware, mw...) }
}

// WithRetryStrategy sets how long the runner waits before retrying after a
// failure. It is reset after every successful batch.
//
// The default is exponential backoff without an upper limit on total time:
// a projection keeps retrying until the cause is fixed. A strategy that
// eventually returns [backoff.Stop] makes the runner wait for its maximum
// interval between retries from then on; Run never gives up.
func WithRetryStrategy(b backoff.BackOff) Option {
	return func(c *config) { c.retry = b }
}

// WithHeartbeat makes the active runner refresh its published status every
// interval while nothing else changes, in addition to the writes on every
// phase change.
//
// Heartbeats let other processes tell a projection that is live but idle
// apart from one whose instance has died; see [MaxAge]. They are off by
// default because each one is a write to the checkpoint store. Pick an
// interval that suits the number of projections: with many projections on
// one database, a short interval adds noticeable load. Requires a
// checkpoint store that implements [StatusStore].
func WithHeartbeat(interval time.Duration) Option {
	return func(c *config) { c.heartbeat = interval }
}

// WithErrorPolicy sets what the runner does when a handler fails on an
// event. The default is [StallOnError].
//
// A failed batch is rolled back as a whole, so the runner first retries
// its events one at a time, each in its own unit of work. That moves the
// checkpoint up to just before the failing event, and lets the error in
// [Status.LastError] name that event's position. Under a skipping policy,
// the failing event is then committed as skipped: recorded through
// [DeadLetterStore] when the checkpoint store implements it, logged
// otherwise, and counted in [Status.Skipped].
func WithErrorPolicy(p ErrorPolicy) Option {
	return func(c *config) { c.errorPolicy = p }
}

// WithOnCatchingUp registers fn to run when the projection starts catching
// up: on a fresh start, after a rebuild, or after falling behind. It runs
// before the first batch of that catch-up.
//
// fn runs outside any batch's unit of work, so it can do things that cannot
// run inside a transaction, such as dropping indexes before a rebuild. The
// [Status] tells the causes apart: a rebuild or fresh start begins at
// Position 0.
//
//	projection.WithOnCatchingUp(func(ctx context.Context, s projection.Status) error {
//		if s.Position > 0 {
//			return nil // only behind, not rebuilding
//		}
//		return projector.DropIndexes(ctx)
//	})
//
// An error from fn is handled like a failed batch: the runner stalls, backs
// off and calls fn again, so fn must be safe to repeat.
func WithOnCatchingUp(fn func(ctx context.Context, s Status) error) Option {
	return func(c *config) { c.onCatchingUp = fn }
}

// WithOnLive registers fn to run when the projection becomes live, after
// the batch that reached the head of the log is committed, and before
// [Runner.Live] reports true. Use it to undo what [WithOnCatchingUp] did,
// such as recreating indexes.
//
// The same rules apply as for WithOnCatchingUp: fn runs outside a unit of
// work, and an error stalls the runner and makes it call fn again.
func WithOnLive(fn func(ctx context.Context, s Status) error) Option {
	return func(c *config) { c.onLive = fn }
}
