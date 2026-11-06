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

// NewTasksToArchive returns an empty list whose tasks become due delay
// after they were completed.
func NewTasksToArchive(delay time.Duration) *TasksToArchive {
	return &TasksToArchive{delay: delay, items: map[uuid.UUID]*item{}}
}

// NewProjection returns the projection that keeps list: a completed task
// joins the list, due its delay after completion; an archived one leaves
// it. Each event type has its own handler, routed by the group.
func NewProjection(list *TasksToArchive) projection.Definition {
	group := eventsourcing.NewEventGroupProcessor(
		eventsourcing.OnEvent(list.onCompleted),
		eventsourcing.OnEvent(list.onArchived),
	)
	return projection.Definition{
		Name:       "tasks-to-archive",
		Handler:    group,
		EventTypes: group.StreamFilter(),
	}
}

func (l *TasksToArchive) onCompleted(_ context.Context, e *events.TaskCompleted) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.items[e.TaskID] = &item{due: e.CompletedAt.Add(l.delay)}
	return nil
}

func (l *TasksToArchive) onArchived(_ context.Context, e *events.TaskArchived) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.items, e.TaskID)
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
