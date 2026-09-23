package completetask

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
)

type CompleteTask struct {
	TaskID      uuid.UUID
	CompletedBy uuid.UUID
}

func (c CompleteTask) AggregateID() string { return c.TaskID.String() }

// taskState tracks everything needed to enforce completion rules.
type taskState struct {
	Exists    bool
	Completed bool
}

var initialState = func() taskState { return taskState{} }

// evolve handles events from both CreateTask and CompleteTask handlers —
// they all write to the same stream.
func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
	switch envelope.Event.(type) {
	case *events.TaskCreated:
		return taskState{Exists: true, Completed: false}
	case *events.TaskCompleted:
		return taskState{Exists: true, Completed: true}
	}
	return state
}

func decide(state taskState, cmd CompleteTask) ([]eventsourcing.Event, error) {
	if !state.Exists {
		return nil, fmt.Errorf("task %s does not exist", cmd.TaskID)
	}
	if state.Completed {
		return nil, fmt.Errorf("task %s is already completed", cmd.TaskID)
	}

	return []eventsourcing.Event{
		&events.TaskCompleted{
			TaskID:      cmd.TaskID,
			CompletedBy: cmd.CompletedBy,
			CompletedAt: time.Now(),
		},
	}, nil
}

func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CompleteTask] {
	return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
