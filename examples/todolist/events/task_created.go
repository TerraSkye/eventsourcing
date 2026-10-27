package events

import (
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

var _ eventsourcing.Event = (*TaskCreated)(nil)

func init() {
	eventsourcing.RegisterEvent(&TaskCreated{})
}

// TaskCreated is emitted when a new task is added to the list.
type TaskCreated struct {
	TaskID      uuid.UUID `json:"task_id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

func (e *TaskCreated) AggregateID() string { return e.TaskID.String() }
func (e *TaskCreated) EventType() string   { return "TaskCreated" }
