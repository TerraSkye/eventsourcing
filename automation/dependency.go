package automation

import (
	"context"

	"github.com/terraskye/eventsourcing/projection"
)

// Dependency reports whether a projection is live, wherever it runs. An
// automation waits for its dependencies before working, so it never acts on
// a read model that is still catching up.
//
// [*projection.Runner] implements it for a projection run by this program,
// and [*projection.Probe] for one run by another program.
type Dependency interface {
	// Name returns the projection's name, as shown in [Status.WaitingFor].
	Name() string

	// IsLive reports whether the projection has caught up with the head of
	// the log.
	IsLive(ctx context.Context) (bool, error)
}

var (
	_ Dependency = (*projection.Runner)(nil)
	_ Dependency = (*projection.Probe)(nil)
	_ Watcher    = (*projection.Runner)(nil)
)

// Watcher is optionally implemented by a [Dependency] that can call back
// when its projection changes, so the automation does not have to poll.
// [*projection.Runner] implements it.
type Watcher interface {
	// Watch registers fn to be called whenever the projection commits new
	// events or changes phase. fn must not block.
	Watch(fn func())
}
