package tasklist_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/tasklist"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestTaskList covers the task-list view: a created task is listed open,
// a completed one is marked completed, and an archived one leaves the list.
func TestTaskList(t *testing.T) {
	groceries, taxes := uuid.New(), uuid.New()
	createGroceries := &events.TaskCreated{TaskID: groceries, Title: "Buy groceries"}
	createTaxes := &events.TaskCreated{TaskID: taxes, Title: "File taxes"}

	tests := []struct {
		name  string
		given []eventsourcing.Event
		want  []tasklist.Task
	}{
		{name: "empty"},
		{
			name:  "lists created tasks open",
			given: []eventsourcing.Event{createGroceries, createTaxes},
			want: []tasklist.Task{
				{ID: groceries.String(), Title: "Buy groceries"},
				{ID: taxes.String(), Title: "File taxes"},
			},
		},
		{
			name:  "marks a completed task",
			given: []eventsourcing.Event{createGroceries, &events.TaskCompleted{TaskID: groceries}},
			want:  []tasklist.Task{{ID: groceries.String(), Title: "Buy groceries", Completed: true}},
		},
		{
			name: "drops an archived task",
			given: []eventsourcing.Event{
				createGroceries, createTaxes,
				&events.TaskCompleted{TaskID: taxes}, &events.TaskArchived{TaskID: taxes},
			},
			want: []tasklist.Task{{ID: groceries.String(), Title: "Buy groceries"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			list := tasklist.NewTaskList()
			slicetest.View(t, tasklist.NewProjection(list)).
				Given(tt.given...).
				Then(func(t testing.TB) {
					got, err := tasklist.NewQueryHandler(list).HandleQuery(t.Context(), tasklist.ListTasks{})
					if err != nil {
						t.Fatal(err)
					}
					slices.SortFunc(got, func(a, b tasklist.Task) int { return strings.Compare(a.Title, b.Title) })
					if !slices.Equal(got, tt.want) {
						t.Fatalf("list = %+v, want %+v", got, tt.want)
					}
				})
		})
	}
}
