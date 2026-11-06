package archivetasks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/automations/archivetasks"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/completetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/createtask"
	"github.com/terraskye/eventsourcing/memory"
)

// recordingBus is a Dispatcher that records the commands it gets and
// answers with err.
type recordingBus struct {
	sent []eventsourcing.Command
	err  error
}

func (b *recordingBus) Dispatch(_ context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
	b.sent = append(b.sent, cmd)
	return eventsourcing.AppendResult{}, b.err
}

func TestWork_DispatchesArchiveTask(t *testing.T) {
	h := &recordingBus{}
	id := uuid.New()
	if err := archivetasks.New(nil, nil, h).Work(t.Context(), id); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(h.sent) != 1 || h.sent[0] != (archivetask.ArchiveTask{TaskID: id}) {
		t.Fatalf("sent %+v, want one ArchiveTask for %s", h.sent, id)
	}
}

func TestWork_ReturnsTheCommandError(t *testing.T) {
	// The runner counts the error as a failure; the item is claimed again
	// once its lease expires.
	want := errors.New("store unavailable")
	h := &recordingBus{err: want}
	if err := archivetasks.New(nil, nil, h).Work(t.Context(), uuid.New()); !errors.Is(err, want) {
		t.Fatalf("Work = %v, want %v", err, want)
	}
}

func TestDefinition_WorksThroughWork(t *testing.T) {
	h := &recordingBus{}
	d := archivetasks.New(nil, nil, h).Definition()
	if d.Name != "archive-tasks" || d.Work == nil {
		t.Fatalf("definition = %+v, want archive-tasks with Work", d)
	}
	if err := d.Work(t.Context(), uuid.New()); err != nil || len(h.sent) != 1 {
		t.Fatalf("Definition.Work = %v after %d commands, want one ArchiveTask", err, len(h.sent))
	}
}

// TestWork_ArchivesACompletedTask runs Work through a command bus with the
// real handler on a memory store: the task's stream ends in TaskArchived, and
// archiving it again, as after a lease expired, appends nothing.
func TestWork_ArchivesACompletedTask(t *testing.T) {
	ctx := t.Context()
	store := memory.NewEventStore()
	defer store.Close()

	id := uuid.New()
	if _, err := createtask.NewHandler(store)(ctx, createtask.CreateTask{TaskID: id, Title: "Buy groceries"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := completetask.NewHandler(store)(ctx, completetask.CompleteTask{TaskID: id}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	bus := eventsourcing.NewCommandBus(10, 1)
	defer bus.Stop()
	eventsourcing.Register(bus, archivetask.NewHandler(store))

	a := archivetasks.New(nil, nil, bus)
	for range 2 {
		if err := a.Work(ctx, id); err != nil {
			t.Fatalf("Work: %v", err)
		}
	}

	it, err := store.LoadStream(ctx, id.String())
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	envs, err := it.All()
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	archived := 0
	for _, e := range envs {
		if _, ok := e.Event.(*events.TaskArchived); ok {
			archived++
		}
	}
	if archived != 1 || len(envs) != 3 {
		t.Fatalf("stream has %d events with %d TaskArchived, want created, completed, archived once", len(envs), archived)
	}
}
