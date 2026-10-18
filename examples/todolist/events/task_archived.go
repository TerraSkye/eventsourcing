package events

import (
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.Event = (*TaskArchived)(nil)

func init() {
	cqrs.RegisterEvent(&TaskArchived{})
}

// TaskArchived is emitted when a completed task is auto-archived.
type TaskArchived struct {
	TaskID     uuid.UUID `json:"task_id"`
	ArchivedAt time.Time `json:"archived_at"`
}

func (e *TaskArchived) AggregateID() string { return e.TaskID.String() }
func (e *TaskArchived) EventType() string   { return "TaskArchived" }
