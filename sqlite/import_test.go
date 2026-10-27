package sqlite_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/sqlite"
)

func TestImport_CopiesTheLogInOrder(t *testing.T) {
	src := memory.NewEventStore()
	t.Cleanup(func() { src.Close() })
	save := func(id string, revision cqrs.StreamState, events ...cqrs.Event) {
		t.Helper()
		envs := make([]cqrs.Envelope, len(events))
		for i, e := range events {
			envs[i] = cqrs.Envelope{
				EventID:    uuid.New(),
				StreamID:   id,
				Event:      e,
				Metadata:   map[string]any{"by": "test"},
				OccurredAt: time.Now(),
			}
		}
		if _, err := src.Save(t.Context(), envs, revision); err != nil {
			t.Fatalf("Save to source: %v", err)
		}
	}
	save("task-1", cqrs.NoStream{}, &taskCreated{ID: "1"})
	save("task-2", cqrs.NoStream{}, &taskCreated{ID: "2"})
	save("task-1", cqrs.Revision(1), &taskDone{ID: "1"})

	dst := sqlite.NewEventStore(openDB(t, ":memory:"))
	n, err := sqlite.Import(t.Context(), src, dst)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if n != 3 {
		t.Fatalf("Import copied %d events, want 3", n)
	}

	want, err := src.ReadAll(t.Context(), 0, cqrs.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadAll source: %v", err)
	}
	got, err := dst.ReadAll(t.Context(), 0, cqrs.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadAll destination: %v", err)
	}
	if len(got.Events) != len(want.Events) {
		t.Fatalf("destination holds %d events, want %d", len(got.Events), len(want.Events))
	}
	for i, w := range want.Events {
		g := got.Events[i]
		if g.EventID != w.EventID || g.StreamID != w.StreamID ||
			g.GlobalPosition != w.GlobalPosition || g.Event.EventType() != w.Event.EventType() ||
			g.Metadata["by"] != "test" || !g.OccurredAt.Equal(w.OccurredAt) {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}
	// Stream positions follow the order within each stream.
	for i, wantVersion := range []uint64{1, 1, 2} {
		if v := got.Events[i].Version; v != wantVersion {
			t.Errorf("event %d has Version %d, want %d", i, v, wantVersion)
		}
	}
}

func TestImport_RefusesAnOccupiedStream(t *testing.T) {
	src := memory.NewEventStore()
	t.Cleanup(func() { src.Close() })
	if _, err := src.Save(t.Context(), []cqrs.Envelope{{StreamID: "task-1", Event: &taskCreated{ID: "1"}}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save to source: %v", err)
	}
	dst := sqlite.NewEventStore(openDB(t, ":memory:"))
	appendEvents(t, dst, "task-1", &taskCreated{ID: "other"})

	_, err := sqlite.Import(t.Context(), src, dst)
	var conflict *cqrs.StreamRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("Import into an occupied stream = %v, want a StreamRevisionConflictError", err)
	}
}
