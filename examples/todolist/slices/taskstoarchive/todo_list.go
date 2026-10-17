// Package taskstoarchive is the todo list of Part 5: every completed task
// that is not archived yet, with the moment it becomes due.
package taskstoarchive

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/projection"
)

type item struct {
	due          time.Time // completed at + delay
	claimedUntil time.Time // leased to the automation until then
}

// TasksToArchive is the todo list: written by the projection, claimed by
// the automation.
type TasksToArchive struct {
	mu    sync.Mutex
	delay time.Duration
	items map[uuid.UUID]*item
}

func NewTasksToArchive(delay time.Duration) *TasksToArchive {
	return &TasksToArchive{delay: delay, items: map[uuid.UUID]*item{}}
}

func NewProjection(list *TasksToArchive) projection.Definition {
	return projection.Definition{
		Name:       "tasks-to-archive",
		Handler:    eventsourcing.NewEventHandlerFunc(list.handle),
		EventTypes: []string{"TaskCompleted", "TaskArchived"},
	}
}

func (l *TasksToArchive) handle(ctx context.Context, event eventsourcing.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch e := event.(type) {
	case *events.TaskCompleted:
		l.items[e.TaskID] = &item{due: e.CompletedAt.Add(l.delay)}
	case *events.TaskArchived:
		delete(l.items, e.TaskID)
	}
	return nil
}

// Claim leases up to max due tasks, so that a second claim skips them until
// the lease runs out.
func (l *TasksToArchive) Claim(ctx context.Context, max int, lease time.Duration) ([]uuid.UUID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	due := []uuid.UUID{}
	for id, it := range l.items {
		if !it.due.After(now) && !it.claimedUntil.After(now) {
			due = append(due, id)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].String() < due[j].String() })
	if len(due) > max {
		due = due[:max]
	}
	for _, id := range due {
		l.items[id].claimedUntil = now.Add(lease)
	}
	return due, nil
}
