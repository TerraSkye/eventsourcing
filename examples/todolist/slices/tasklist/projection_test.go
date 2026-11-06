package tasklist_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/tasklist"
)

func TestProjection_KeepsTheTaskList(t *testing.T) {
	ctx := t.Context()
	list := tasklist.NewTaskList()
	p := tasklist.NewProjection(list)
	if want := []string{"TaskArchived", "TaskCompleted", "TaskCreated"}; !slices.Equal(p.EventTypes, want) {
		t.Fatalf("EventTypes = %v, want %v", p.EventTypes, want)
	}

	keep, done := uuid.New(), uuid.New()
	for _, e := range []eventsourcing.Event{
		&events.TaskCreated{TaskID: keep, Title: "Buy groceries"},
		&events.TaskCreated{TaskID: done, Title: "File taxes"},
		&events.TaskCompleted{TaskID: keep},
		&events.TaskCompleted{TaskID: done},
		&events.TaskArchived{TaskID: done},
	} {
		if err := p.Handler.Handle(ctx, e); err != nil {
			t.Fatalf("Handle(%T): %v", e, err)
		}
	}

	got, err := tasklist.NewQueryHandler(list).HandleQuery(context.Background(), tasklist.ListTasks{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != keep.String() || got[0].Title != "Buy groceries" || !got[0].Completed {
		t.Fatalf("list = %+v, want only the completed, unarchived task %s", got, keep)
	}
}
