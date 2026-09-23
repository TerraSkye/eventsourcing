package createtask

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
)

// CreateTask is the command — present tense, expresses intent.
type CreateTask struct {
	TaskID      uuid.UUID
	Title       string
	Description string
	CreatedBy   uuid.UUID
}

func (c CreateTask) AggregateID() string { return c.TaskID.String() }

// taskState is the minimal state needed to enforce business rules.
type taskState struct {
	Exists bool
}

// initialState is a function that returns a fresh state — required because
// the framework calls it to create a new instance each time.
var initialState = func() taskState {
	return taskState{}
}

// evolve rebuilds state from a single event. It must be a pure function.
func evolve(state taskState, envelope *eventsourcing.Envelope) taskState {
	switch envelope.Event.(type) {
	case *events.TaskCreated:
		return taskState{Exists: true}
	}
	return state
}

// decide enforces business rules and returns the events to persist.
// Return an error to reject the command.
func decide(state taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
	if state.Exists {
		return nil, fmt.Errorf("task %s already exists", cmd.TaskID)
	}
	if cmd.Title == "" {
		return nil, fmt.Errorf("title is required")
	}

	return []eventsourcing.Event{
		&events.TaskCreated{
			TaskID:      cmd.TaskID,
			Title:       cmd.Title,
			Description: cmd.Description,
			CreatedBy:   cmd.CreatedBy,
			CreatedAt:   time.Now(),
		},
	}, nil
}

// NewHandler wires the three pieces together.
func NewHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[CreateTask] {
	return eventsourcing.NewCommandHandler(store, initialState, evolve, decide)
}
