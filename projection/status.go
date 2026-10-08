package projection

import (
	"context"
	"time"
)

// Phase is where a projection is in its life cycle.
type Phase int

const (
	// Starting means [Runner.Run] has not loaded the checkpoint yet.
	Starting Phase = iota

	// Standby means another instance runs the projection; this runner
	// waits to take over if that instance goes away. See [Locker].
	Standby

	// CatchingUp means the projection is behind the head of the log and
	// reading full batches: on a fresh start, during a rebuild, or after
	// falling behind.
	CatchingUp

	// Live means the last read reached the head of the log. The read model
	// reflects every event appended up to that read.
	Live

	// Stalled means the last attempt failed; the runner is backing off
	// before it retries. [Status.LastError] says why.
	Stalled
)

// String returns the phase's name in lower case, as stored by a
// [StatusStore]: "starting", "standby", "catching_up", "live" or
// "stalled".
func (p Phase) String() string {
	switch p {
	case Starting:
		return "starting"
	case Standby:
		return "standby"
	case CatchingUp:
		return "catching_up"
	case Live:
		return "live"
	case Stalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// Status describes a projection's progress, as reported by
// [Runner.Status] and stored by a [StatusStore].
type Status struct {
	// Phase is where the projection is in its life cycle.
	Phase Phase

	// Position is the checkpoint: the GlobalVersion up to which the
	// projection has processed the log.
	Position uint64

	// Head is the head of the log as of the last read; Head minus Position
	// is how far the projection lags behind.
	Head uint64

	// Version is the projector version the read model was built with; see
	// [WithVersion].
	Version int

	// Replaying reports that the projection is processing events it had
	// already processed before its last rebuild, or that were already in
	// the log when it first started. See [IsReplaying].
	Replaying bool

	// Skipped counts the events skipped under [WithErrorPolicy] since the
	// runner started.
	Skipped uint64

	// LastError is the error that stalled the projection, or "" when it is
	// not [Stalled].
	LastError string

	// Owner identifies the instance running the projection, as reported by
	// the [Locker]; "" when there is no locker.
	Owner string

	// Age is how long ago this status was published, measured by the
	// store's own clock so that clock differences between machines do not
	// matter. It is only set on a status returned by
	// [StatusStore.LoadStatus], and is zero otherwise.
	Age time.Duration
}

// Dependency reports whether a projection is live, wherever it runs.
// Automations take Dependencies, so they can wait for the read model they
// work from without knowing whether it runs in the same process.
//
// [*Runner] is a Dependency for a projection run by this program, on this
// instance or another one. [Remote] is a Dependency for a projection run by
// a different program.
type Dependency interface {
	// Name returns the projection's name.
	Name() string

	// Live reports whether the projection is caught up with the head of the
	// log. An error means liveness could not be determined; callers should
	// treat it as not live.
	Live(ctx context.Context) (bool, error)
}

// Watcher is optionally implemented by a [Dependency] that can call back
// when its projection changes, so dependents learn about new work without
// waiting for their next poll. [*Runner] implements it.
type Watcher interface {
	// Watch registers fn to be called whenever the projection committed new
	// events or changed [Phase]. fn must not block.
	Watch(fn func())
}

// RemoteOption configures a [Remote] dependency.
type RemoteOption func(*remoteConfig)

type remoteConfig struct {
	maxAge     time.Duration
	minVersion int
}

// MaxAge treats the projection as not live when its published status is
// older than d. Without it, the last published status is trusted however
// old it is.
//
// Use it together with [WithHeartbeat] on the runner, and set d to about
// three heartbeat intervals: one late heartbeat then does not pause the
// dependents, but an instance that died does within seconds.
func MaxAge(d time.Duration) RemoteOption {
	return func(c *remoteConfig) { c.maxAge = d }
}

// MinVersion treats the projection as not live until its read model was
// built by projector version v or later; see [WithVersion].
//
// It protects dependents during a rolling deploy: code that expects the
// read model of version 3 waits while the projection is still at version 2
// or is being rebuilt to version 3.
func MinVersion(v int) RemoteOption {
	return func(c *remoteConfig) { c.minVersion = v }
}

// Remote returns a [Dependency] for the projection called name, run by a
// different program, judged by the status its runner publishes to store.
//
// store is a [StatusStore] for the database that holds the projection's
// checkpoint, which is usually the database the dependent reads the read
// model from anyway:
//
//	shipping := automation.New("ship-orders",
//		projection.Remote("orders-to-ship", checkpoints, projection.MaxAge(15*time.Second)),
//		claim, work,
//	)
//
// The projection counts as live when its published [Phase] is [Live], and
// the [MaxAge] and [MinVersion] conditions hold. A projection that has never
// published a status is not live.
func Remote(name string, store StatusStore, opts ...RemoteOption) Dependency {
	panic(errNotImplemented)
}
