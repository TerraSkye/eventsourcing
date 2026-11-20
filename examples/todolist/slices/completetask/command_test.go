package completetask_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/completetask"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestCompleteTask covers the complete-task slice: an open task is
// completed; a task that does not exist, or is already completed, is
// rejected and records nothing.
func TestCompleteTask(t *testing.T) {
	id, user := uuid.New(), uuid.New()
	created := &events.TaskCreated{TaskID: id, Title: "Buy groceries"}
	complete := completetask.CompleteTask{TaskID: id, CompletedBy: user}

	tests := []struct {
		name    string
		given   []eventsourcing.Event
		when    completetask.CompleteTask
		wantErr error
		want    []eventsourcing.Event
	}{
		{
			name:  "completes an open task",
			given: []eventsourcing.Event{created},
			when:  complete,
			want:  []eventsourcing.Event{&events.TaskCompleted{TaskID: id, CompletedBy: user}},
		},
		{
			name:    "rejects a task that does not exist",
			when:    complete,
			wantErr: slicetest.ErrRejected,
		},
		{
			name:    "rejects a completed task",
			given:   []eventsourcing.Event{created, &events.TaskCompleted{TaskID: id}},
			when:    complete,
			wantErr: slicetest.ErrRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slicetest.Command(t, completetask.NewHandler, slicetest.IgnoreFields("CompletedAt")).
				Given(tt.given...).
				When(tt.when).
				Expect(tt.wantErr, tt.want...)
		})
	}
}
