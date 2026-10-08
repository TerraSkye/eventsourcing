package automation

import (
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// Option configures an [Automation]; pass options to [New].
type Option func(*config)

type config struct {
	interval  time.Duration
	batchSize int
	lease     time.Duration
	waitFor   []projection.Dependency
}

func defaultConfig() config {
	return config{
		interval:  5 * time.Second,
		batchSize: 10,
		lease:     time.Minute,
	}
}

// WithInterval sets how long an idle automation waits before checking for
// work again when nothing nudges it. The default is five seconds.
//
// With a todo list in the same program, nudges usually wake the automation
// long before the interval elapses, and the interval is only a safety net.
// With a [projection.Remote] todo list, the interval is what decides how
// quickly new work is picked up.
func WithInterval(d time.Duration) Option {
	return func(c *config) { c.interval = d }
}

// WithBatchSize sets how many items are claimed at once. The default is 10.
//
// Items of a batch are processed one after the other, and all of them are
// leased from the moment of the claim, so the batch size times the time
// one item takes must stay well below the lease; see [WithLease].
func WithBatchSize(n int) Option {
	return func(c *config) { c.batchSize = n }
}

// WithLease sets how long claimed items are reserved for this instance.
// The default is one minute.
//
// The lease is what makes a crashed automation's items available again: an
// item whose work did not lead to its removal from the todo list within
// the lease is claimed again. Too short a lease makes slow work run twice
// concurrently; too long a lease delays the retry after a crash.
func WithLease(d time.Duration) Option {
	return func(c *config) { c.lease = d }
}

// WaitFor adds projections that must be live, besides the todo list, before
// the automation claims work. Use it when the work reads other read models
// that must be up to date for it to make the right decision.
func WaitFor(deps ...projection.Dependency) Option {
	return func(c *config) { c.waitFor = append(c.waitFor, deps...) }
}
