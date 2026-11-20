package createtask_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/createtask"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestCreateTask covers the create-task slice: a new task with a title is
// created; a task that already exists, or one without a title, is
// rejected and records nothing.
func TestCreateTask(t *testing.T) {
	id, author := uuid.New(), uuid.New()
	create := createtask.CreateTask{TaskID: id, Title: "Buy groceries", Description: "Milk", CreatedBy: author}

	tests := []struct {
		name    string
		given   []eventsourcing.Event
		when    createtask.CreateTask
		wantErr error
		want    []eventsourcing.Event
	}{
		{
			name: "creates the task",
			when: create,
			want: []eventsourcing.Event{
				&events.TaskCreated{TaskID: id, Title: "Buy groceries", Description: "Milk", CreatedBy: author},
			},
		},
		{
			name:    "rejects a task that exists",
			given:   []eventsourcing.Event{&events.TaskCreated{TaskID: id, Title: "Buy groceries"}},
			when:    create,
			wantErr: slicetest.ErrRejected,
		},
		{
			name:    "rejects a task without a title",
			when:    createtask.CreateTask{TaskID: id},
			wantErr: slicetest.ErrRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slicetest.Command(t, createtask.NewHandler, slicetest.IgnoreFields("CreatedAt")).
				Given(tt.given...).
				When(tt.when).
				Expect(tt.wantErr, tt.want...)
		})
	}
}
