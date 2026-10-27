package sqlite_test

// Event types and helpers the package's tests share.

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/sqlite"
)

type taskCreated struct {
	ID string
}

func (e *taskCreated) AggregateID() string { return e.ID }
func (e *taskCreated) EventType() string   { return "sqlite_test.TaskCreated" }

type taskDone struct {
	ID string
}

func (e *taskDone) AggregateID() string { return e.ID }
func (e *taskDone) EventType() string   { return "sqlite_test.TaskDone" }

func init() {
	eventsourcing.RegisterEvent(&taskCreated{})
	eventsourcing.RegisterEvent(&taskDone{})
}

// appendEvents saves events to stream id and returns the last position.
func appendEvents(t *testing.T, store *sqlite.EventStore, id string, events ...eventsourcing.Event) uint64 {
	t.Helper()
	envs := make([]eventsourcing.Envelope, len(events))
	for i, e := range events {
		envs[i] = eventsourcing.Envelope{EventID: uuid.New(), StreamID: id, Event: e, OccurredAt: time.Now()}
	}
	res, err := store.Save(t.Context(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return res.GlobalPosition
}

// waitFor fails the test unless cond holds within two seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
