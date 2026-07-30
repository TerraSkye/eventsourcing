package events

import (
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.Event = (*TaskCompleted)(nil)

func init() {
	cqrs.RegisterEvent(&TaskCompleted{})
}

// TaskCompleted is emitted when a task is marked as done.
type TaskCompleted struct {
	TaskID      uuid.UUID `json:"task_id"`
	CompletedBy uuid.UUID `json:"completed_by"`
	CompletedAt time.Time `json:"completed_at"`
}

func (e *TaskCompleted) AggregateID() string { return e.TaskID.String() }
func (e *TaskCompleted) EventType() string   { return "TaskCompleted" }
