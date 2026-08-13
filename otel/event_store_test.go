package otel

import (
	"context"
	"sync"
	"testing"

	"github.com/terraskye/eventsourcing"
)

// TestSave_ConcurrentCallsSharingMetadataMapRace verifies that
// TelemetryStore.Save clones each Envelope's Metadata map before stamping
// tracing/causation data into it, rather than mutating the caller-supplied
// map in place (events[i].Metadata["causation_id"] = ..., etc.). Callers
// commonly build one "base" metadata map once (e.g. a tenant or request ID)
// and attach it, unmodified, to several Envelopes — cqrs.NewCommandHandler's
// own WithMetadataExtractor relies on exactly this being safe, calling
// maps.Clone(baseMetadata) per Envelope before Save. Without cloning here
// too, any caller of the EventStore interface directly (bypassing
// CommandHandler's cloning) that shares one Metadata map across Envelopes
// saved concurrently would hit an unsynchronized concurrent write to that
// shared Go map from two Save calls
// at once.
func TestSave_ConcurrentCallsSharingMetadataMapRace(t *testing.T) {
	shared := map[string]any{"tenant": "t1"}
	stub := &loadFromAllStub{}
	store := WithEventStoreTelemetry(stub)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ctx := eventsourcing.WithCausation(context.Background(), "causation-id")
			events := []eventsourcing.Envelope{
				{
					StreamID: "agg-1",
					Event:    loadFromAllStubEvent{},
					Version:  uint64(n),
					Metadata: shared,
				},
			}
			_, _ = store.Save(ctx, events, eventsourcing.Any{})
		}(i)
	}
	wg.Wait()
}

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

// TestWithEventStoreTelemetry_WithOperationIgnoredForSaveSpan verifies
// WithEventStoreTelemetry's doc comment promise that "Options such as
// [WithAttributes] and [WithOperation] customize the spans produced" holds
// for TelemetryStore.Save's span, not just its hardcoded default name
// "append eventstore".
func TestWithEventStoreTelemetry_WithOperationIgnoredForSaveSpan(t *testing.T) {
	const wantSpanName = "custom-save-operation"

	stub := &loadFromAllStub{}
	store := WithEventStoreTelemetry(stub, WithOperation(wantSpanName))

	before := spanNameRecorder.len()

	_, err := store.Save(context.Background(), []eventsourcing.Envelope{
		{StreamID: "agg-1", Event: loadFromAllStubEvent{}, Version: 1},
	}, eventsourcing.NoStream{})
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got := spanNameRecorder.since(before)
	if len(got) == 0 {
		t.Fatalf("no span was started for Save()")
	}

	found := false
	for _, name := range got {
		if name == wantSpanName {
			found = true
		}
	}
	if !found {
		t.Fatalf("WithOperation(%q) had no effect; span(s) started: %v (want a span named %q, per WithEventStoreTelemetry's doc comment)", wantSpanName, got, wantSpanName)
	}
}
