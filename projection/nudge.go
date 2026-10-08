package projection

import (
	"context"

	"github.com/terraskye/eventsourcing"
)

// Nudger is anything that can be asked to check for new work right away;
// [*Runner] is one, and so is an automation.
type Nudger interface {
	// Nudge asks for a check without blocking. Repeated nudges may be
	// coalesced into one check.
	Nudge()
}

// Nudgers is a group of [Nudger] values that is itself a Nudger, so that one
// call wakes them all.
type Nudgers []Nudger

// Nudge nudges every member of the group.
func (ns Nudgers) Nudge() {
	for _, n := range ns {
		n.Nudge()
	}
}

// NudgeOnSave returns [eventsourcing.EventStoreMiddleware] that nudges
// targets after every successful Save, so projections in this process see
// new events within milliseconds instead of at their next poll.
//
// Wrap the store the command handlers write through, and give the runners
// the unwrapped store:
//
//	raw := memory.NewMemoryStore(100)
//
//	taskList := projection.NewRunner("task-list", projector.EventHandlers(), raw)
//
//	store := projection.NudgeOnSave(taskList)(raw)
//	createTask := createtask.NewHandler(store)
//
// Nudges only reach runners in this process; runners elsewhere rely on the
// store's [eventsourcing.Notifier] or on polling.
func NudgeOnSave(targets ...Nudger) eventsourcing.EventStoreMiddleware {
	group := Nudgers(targets)
	return func(next eventsourcing.EventStore) eventsourcing.EventStore {
		return &nudgingStore{EventStore: next, targets: group}
	}
}

type nudgingStore struct {
	eventsourcing.EventStore
	targets Nudgers
}

func (s *nudgingStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	res, err := s.EventStore.Save(ctx, events, revision)
	if err == nil {
		s.targets.Nudge()
	}
	return res, err
}
