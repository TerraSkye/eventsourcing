package archivetask

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
)

type ArchiveTask struct {
	TaskID uuid.UUID
}

func (c ArchiveTask) AggregateID() string { return c.TaskID.String() }

type taskState struct {
	Completed bool
	Archived  bool
}

var initialState = func() taskState { return taskState{} }

func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
	switch envelope.Event.(type) {
	case *events.TaskCompleted:
		return taskState{Completed: true}
	case *events.TaskArchived:
		return taskState{Completed: true, Archived: true}
	}
	return state
}

func decide(state taskState, cmd ArchiveTask) ([]eventsourcing.Event, error) {
	if !state.Completed {
		return nil, fmt.Errorf("task %s is not completed", cmd.TaskID)
	}
	if state.Archived {
		return nil, nil // idempotent — already archived, no events
	}

	return []eventsourcing.Event{
		&events.TaskArchived{
			TaskID:     cmd.TaskID,
			ArchivedAt: time.Now(),
		},
	}, nil
}

func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[ArchiveTask] {
	return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
