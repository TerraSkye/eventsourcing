package tasklist

import (
	"context"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/projection"
)

// NewProjection returns the projection that keeps list up to date (Part 4).
// Each event type has its own handler; the group routes every event to the
// one for its type, and its StreamFilter tells the runner which event
// types to read. Archived tasks leave the list.
func NewProjection(list *TaskList) projection.Definition {
	group := eventsourcing.NewEventGroupProcessor(
		eventsourcing.OnEvent(list.onCreated),
		eventsourcing.OnEvent(list.onCompleted),
		eventsourcing.OnEvent(list.onArchived),
	)
	return projection.Definition{
		Name:       "task-list",
		Handler:    group,
		EventTypes: group.StreamFilter(),
	}
}

func (l *TaskList) onCreated(_ context.Context, e *events.TaskCreated) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tasks[e.TaskID.String()] = &Task{ID: e.TaskID.String(), Title: e.Title}
	return nil
}

func (l *TaskList) onCompleted(_ context.Context, e *events.TaskCompleted) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.tasks[e.TaskID.String()]; ok {
		t.Completed = true
	}
	return nil
}

func (l *TaskList) onArchived(_ context.Context, e *events.TaskArchived) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.tasks, e.TaskID.String())
	return nil
}
