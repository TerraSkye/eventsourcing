package tasklist

import (
	"context"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/projection"
)

// NewProjection returns the projection that keeps list up to date (Part 4).
// Archived tasks leave the list.
func NewProjection(list *TaskList) projection.Definition {
	return projection.Definition{
		Name:       "task-list",
		Handler:    eventsourcing.NewEventHandlerFunc(list.handle),
		EventTypes: []string{"TaskCreated", "TaskCompleted", "TaskArchived"},
	}
}

// handle applies one event to the task list. The runner calls it for every
// event in EventTypes, in the order they were stored.
func (l *TaskList) handle(_ context.Context, event eventsourcing.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch e := event.(type) {
	case *events.TaskCreated:
		l.tasks[e.TaskID.String()] = &Task{
			ID:    e.TaskID.String(),
			Title: e.Title,
		}
	case *events.TaskCompleted:
		if t, ok := l.tasks[e.TaskID.String()]; ok {
			t.Completed = true
		}
	case *events.TaskArchived:
		delete(l.tasks, e.TaskID.String())
	}
	return nil
}
