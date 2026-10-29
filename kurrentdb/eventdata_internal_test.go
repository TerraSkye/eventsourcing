package kurrentdb

import (
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

type eventDataProbe struct{ N int }

func (eventDataProbe) AggregateID() string { return "event-data-probe" }
func (eventDataProbe) EventType() string   { return "kurrentdb.eventDataProbe" }

func init() {
	eventsourcing.RegisterEventByType(func() eventsourcing.Event { return &eventDataProbe{} })
}

// TestToEventData_EventID covers the ID Save sends: an Envelope's own
// EventID is kept, and one without gets a new ID before Save's retry loop,
// not one per attempt from the client, so the server can recognise a retry
// of an append that committed.
func TestToEventData_EventID(t *testing.T) {
	t.Run("kept", func(t *testing.T) {
		id := uuid.New()
		got, err := toEventData(eventsourcing.Envelope{EventID: id, Event: &eventDataProbe{N: 1}})
		if err != nil {
			t.Fatalf("toEventData() error = %v", err)
		}
		if got.EventID != id {
			t.Errorf("EventID = %s, want %s", got.EventID, id)
		}
	})

	t.Run("assigned when nil", func(t *testing.T) {
		a, err := toEventData(eventsourcing.Envelope{Event: &eventDataProbe{N: 1}})
		if err != nil {
			t.Fatalf("toEventData() error = %v", err)
		}
		b, err := toEventData(eventsourcing.Envelope{Event: &eventDataProbe{N: 1}})
		if err != nil {
			t.Fatalf("toEventData() error = %v", err)
		}
		if a.EventID == uuid.Nil || b.EventID == uuid.Nil {
			t.Fatalf("EventID = %s, %s; want both set", a.EventID, b.EventID)
		}
		if a.EventID == b.EventID {
			t.Errorf("two events got the same EventID %s", a.EventID)
		}
	})
}
