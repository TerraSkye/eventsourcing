package archivetask_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestArchiveTask covers the archive-task slice: a completed task is
// archived once, archiving it again records nothing, so the automation
// may repeat the command, and a task that is not completed is rejected.
func TestArchiveTask(t *testing.T) {
	id := uuid.New()
	created := &events.TaskCreated{TaskID: id, Title: "Buy groceries"}
	completed := &events.TaskCompleted{TaskID: id}
	archive := archivetask.ArchiveTask{TaskID: id}

	tests := []struct {
		name    string
		given   []eventsourcing.Event
		wantErr error
		want    []eventsourcing.Event
	}{
		{
			name:  "archives a completed task",
			given: []eventsourcing.Event{created, completed},
			want:  []eventsourcing.Event{&events.TaskArchived{TaskID: id}},
		},
		{
			name:  "records nothing for an archived task",
			given: []eventsourcing.Event{created, completed, &events.TaskArchived{TaskID: id}},
		},
		{
			name:    "rejects an open task",
			given:   []eventsourcing.Event{created},
			wantErr: slicetest.ErrRejected,
		},
		{
			name:    "rejects a task that does not exist",
			wantErr: slicetest.ErrRejected,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			slicetest.Command(t, archivetask.NewHandler, slicetest.IgnoreFields("ArchivedAt")).
				Given(tt.given...).
				When(archive).
				Expect(tt.wantErr, tt.want...)
		})
	}
}
