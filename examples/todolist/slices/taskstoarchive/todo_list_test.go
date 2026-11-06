package taskstoarchive_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
)

func TestProjection_ListsCompletedTasksUntilArchived(t *testing.T) {
	ctx := t.Context()
	const delay = time.Hour
	list := taskstoarchive.NewTasksToArchive(delay)
	p := taskstoarchive.NewProjection(list)
	if want := []string{"TaskArchived", "TaskCompleted"}; !slices.Equal(p.EventTypes, want) {
		t.Fatalf("EventTypes = %v, want %v", p.EventTypes, want)
	}

	due, notDue, archived := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()
	for _, e := range []eventsourcing.Event{
		&events.TaskCompleted{TaskID: due, CompletedAt: now.Add(-2 * delay)},
		&events.TaskCompleted{TaskID: notDue, CompletedAt: now},
		&events.TaskCompleted{TaskID: archived, CompletedAt: now.Add(-2 * delay)},
		&events.TaskArchived{TaskID: archived},
	} {
		if err := p.Handler.Handle(ctx, e); err != nil {
			t.Fatalf("Handle(%T): %v", e, err)
		}
	}

	req := automation.ClaimRequest{MaxPerKey: 10, Lease: time.Minute}
	got, err := list.Claim(ctx, req)
	if err != nil || len(got) != 1 || got[0] != due {
		t.Fatalf("Claim = %v, %v; want only the due task %s", got, err, due)
	}
	// Leased, it is not claimed again.
	if again, _ := list.Claim(ctx, req); len(again) != 0 {
		t.Fatalf("second Claim = %v, want nothing while the first lease runs", again)
	}
}
