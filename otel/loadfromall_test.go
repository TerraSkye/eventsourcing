package otel

import (
	"context"
	"testing"

	"github.com/terraskye/eventsourcing"
)

type loadFromAllStubEvent struct{}

func (loadFromAllStubEvent) AggregateID() string { return "agg-1" }
func (loadFromAllStubEvent) EventType() string   { return "stub.event" }

// loadFromAllStub is a minimal EventStore whose LoadFromAll returns a plain
// slice iterator. A slice iterator signals exhaustion with io.EOF, which
// Iterator.Next translates into "Next() == false, Err() == nil".
type loadFromAllStub struct {
	events []*eventsourcing.Envelope
}

func (s *loadFromAllStub) Save(context.Context, []eventsourcing.Envelope, eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	return eventsourcing.AppendResult{}, nil
}

func (s *loadFromAllStub) LoadStream(context.Context, string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(s.events), nil
}

func (s *loadFromAllStub) LoadStreamFrom(context.Context, string, eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(s.events), nil
}

func (s *loadFromAllStub) LoadFromAll(context.Context, eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(s.events), nil
}

func (s *loadFromAllStub) Close() error { return nil }

// TestTelemetryStore_LoadFromAll_TerminatesAtEndOfStream is a regression
// test for GitHub issue #51: LoadFromAll's clean-completion branch only
// returned io.EOF when the inner iterator's Err() itself was io.EOF, but
// Iterator.Next always clears Err() back to nil on a clean end of stream —
// so the guard never fired and the branch fell through to return (nil,
// nil). Iterator.Next treats a nil error as "here is a valid item", so the
// wrapped iterator reported true forever and handed out nil *Envelope
// values instead of terminating.
func TestTelemetryStore_LoadFromAll_TerminatesAtEndOfStream(t *testing.T) {
	const guard = 100 // bounded so a non-terminating iterator fails instead of hanging

	tests := []struct {
		name  string
		count int
	}{
		{name: "empty stream", count: 0},
		{name: "single event", count: 1},
		{name: "several events", count: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &loadFromAllStub{}
			for i := 0; i < tt.count; i++ {
				stub.events = append(stub.events, &eventsourcing.Envelope{
					StreamID: "agg-1",
					Event:    loadFromAllStubEvent{},
					Version:  uint64(i),
				})
			}

			store := WithEventStoreTelemetry(stub)

			iter, err := store.LoadFromAll(context.Background(), eventsourcing.Any{})
			if err != nil {
				t.Fatalf("LoadFromAll() error = %v", err)
			}

			var got int
			for iter.Next(context.Background()) {
				if iter.Value() == nil {
					t.Fatalf("iterator yielded a nil *Envelope at index %d", got)
				}
				got++
				if got > guard {
					t.Fatalf("iterator did not terminate: yielded more than %d values for a %d-event store", guard, tt.count)
				}
			}
			if err := iter.Err(); err != nil {
				t.Fatalf("Err() = %v, want nil", err)
			}
			if got != tt.count {
				t.Errorf("yielded %d events, want %d", got, tt.count)
			}
		})
	}
}
