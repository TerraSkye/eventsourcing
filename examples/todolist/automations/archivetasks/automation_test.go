package archivetasks_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/automations/archivetasks"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestWork_DispatchesArchiveTask covers that Work sends one ArchiveTask
// for the task it is given, through the bus.
func TestWork_DispatchesArchiveTask(t *testing.T) {
	bus := slicetest.NewRecordingDispatcher()
	id := uuid.New()
	if err := archivetasks.New(nil, nil, bus).Work(t.Context(), id); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if got := bus.Dispatched(); len(got) != 1 || got[0] != (archivetask.ArchiveTask{TaskID: id}) {
		t.Fatalf("dispatched %+v, want one ArchiveTask for %s", got, id)
	}
}

// TestWork_ReturnsTheCommandError covers that Work returns the command's
// error: the runner counts it as a failure, and the item is claimed again
// once its lease expires.
func TestWork_ReturnsTheCommandError(t *testing.T) {
	want := errors.New("store unavailable")
	bus := slicetest.NewRecordingDispatcher()
	bus.Reject("ArchiveTask", want)
	if err := archivetasks.New(nil, nil, bus).Work(t.Context(), uuid.New()); !errors.Is(err, want) {
		t.Fatalf("Work = %v, want %v", err, want)
	}
}

// TestDefinition_WorksThroughWork covers that the definition is named
// archive-tasks and works items through Work.
func TestDefinition_WorksThroughWork(t *testing.T) {
	bus := slicetest.NewRecordingDispatcher()
	d := archivetasks.New(nil, nil, bus).Definition()
	if d.Name != "archive-tasks" || d.Work == nil {
		t.Fatalf("definition = %+v, want archive-tasks with Work", d)
	}
	if err := d.Work(t.Context(), uuid.New()); err != nil || len(bus.Dispatched()) != 1 {
		t.Fatalf("Definition.Work = %v after %d commands, want one ArchiveTask", err, len(bus.Dispatched()))
	}
}

// TestArchiveTasks covers the automation across its slices: the todo
// list, the automation and the archive-task handler. A task completed
// longer ago than the delay is archived once; a recently completed or an
// already archived task is left alone.
func TestArchiveTasks(t *testing.T) {
	const delay = 7 * 24 * time.Hour
	id := uuid.New()
	created := &events.TaskCreated{TaskID: id, Title: "Buy groceries"}
	completedLongAgo := &events.TaskCompleted{TaskID: id, CompletedAt: time.Now().Add(-delay - time.Hour)}

	tests := []struct {
		name  string
		given []eventsourcing.Event
		want  []eventsourcing.Event
	}{
		{
			name:  "archives a task completed longer ago than the delay",
			given: []eventsourcing.Event{created, completedLongAgo},
			want:  []eventsourcing.Event{&events.TaskArchived{TaskID: id}},
		},
		{
			name:  "leaves a recently completed task",
			given: []eventsourcing.Event{created, &events.TaskCompleted{TaskID: id, CompletedAt: time.Now()}},
		},
		{
			name:  "leaves an archived task",
			given: []eventsourcing.Event{created, completedLongAgo, &events.TaskArchived{TaskID: id}},
		},
		{name: "leaves an open task", given: []eventsourcing.Event{created}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := slicetest.NewScenario(t, slicetest.IgnoreFields("ArchivedAt"))
			list := taskstoarchive.NewTasksToArchive(delay)
			todo := s.Projection(taskstoarchive.NewProjection(list))
			slicetest.Handle(s, archivetask.NewHandler(s.Store()))
			slicetest.Automation(s, archivetasks.New(list, todo, s.Bus()).Definition())

			s.Given(tt.given...)
			s.Run()
			s.Then(tt.want...)
		})
	}
}
