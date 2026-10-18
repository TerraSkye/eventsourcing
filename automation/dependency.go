package automation

import (
	"context"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// Dependency reports whether a projection is live, wherever it runs. An
// automation waits for its dependencies before working, so it never acts on
// a read model that is still catching up.
//
// [*projection.Runner] implements it for a projection run by this program,
// and [Remote] for one run by another program.
type Dependency interface {
	// Name returns the projection's name, as shown in [Status.WaitingFor].
	Name() string

	// Live reports whether the projection has caught up with the head of
	// the log.
	Live(ctx context.Context) (bool, error)
}

// Watcher is optionally implemented by a [Dependency] that can call back
// when its projection changes, so the automation does not have to poll.
// [*projection.Runner] implements it.
type Watcher interface {
	// Watch registers fn to be called whenever the projection commits new
	// events or changes phase. fn must not block.
	Watch(fn func())
}

// Remote returns a [Dependency] for the projection called name, run by a
// different program, judged by the status its runner publishes to store.
// claimTTL is the [projection.Projection.ClaimTTL] that program runs the
// projection with, or 0 for [projection.DefaultClaimTTL]; a segment whose
// status is older than that counts as not live, as its runner has stopped.
//
// Use it in an automation whose todo-list projection lives in another
// service: the dependency pauses the automation until that projection has
// caught up.
func Remote(name string, store projection.Store, claimTTL time.Duration) Dependency {
	if claimTTL <= 0 {
		claimTTL = projection.DefaultClaimTTL
	}
	return &remote{name: name, store: store, claimTTL: claimTTL}
}

type remote struct {
	name     string
	store    projection.Store
	claimTTL time.Duration
}

func (r *remote) Name() string { return r.name }

func (r *remote) Live(ctx context.Context) (bool, error) {
	statuses, err := r.store.LoadStatuses(ctx, r.name)
	if err != nil || len(statuses) == 0 {
		return false, err
	}
	for _, s := range statuses {
		if !s.IsLive(r.claimTTL) {
			return false, nil
		}
	}
	return true, nil
}

var (
	_ Dependency = (*projection.Runner)(nil)
	_ Watcher    = (*projection.Runner)(nil)
)
