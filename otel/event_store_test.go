package otel

import (
	"context"
	"errors"
	"io"
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

func (s *loadFromAllStub) LoadStream(ctx context.Context, _ string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(ctx, s.events), nil
}

func (s *loadFromAllStub) LoadStreamFrom(ctx context.Context, _ string, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(ctx, s.events), nil
}

func (s *loadFromAllStub) LoadFromAll(ctx context.Context, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(ctx, s.events), nil
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
			for iter.Next() {
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

// closeTrackingStore is an EventStore whose Load* methods return iterators
// that count how many times they are closed and report a read error after
// the first event, so tests can observe both propagation paths.
type closeTrackingStore struct {
	events  []*eventsourcing.Envelope
	closes  int
	readErr error
}

func (s *closeTrackingStore) iter(ctx context.Context) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	i := 0
	return eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*eventsourcing.Envelope, error) {
		if i >= len(s.events) {
			if s.readErr != nil {
				return nil, s.readErr
			}
			return nil, io.EOF
		}
		ev := s.events[i]
		i++
		return ev, nil
	}, func() error { s.closes++; return nil }), nil
}

func (s *closeTrackingStore) Save(context.Context, []eventsourcing.Envelope, eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	return eventsourcing.AppendResult{}, nil
}

func (s *closeTrackingStore) LoadStream(ctx context.Context, _ string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return s.iter(ctx)
}

func (s *closeTrackingStore) LoadStreamFrom(ctx context.Context, _ string, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return s.iter(ctx)
}

func (s *closeTrackingStore) LoadFromAll(ctx context.Context, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return s.iter(ctx)
}

func (s *closeTrackingStore) Close() error { return nil }

// TestTelemetryStore_ClosesUnderlyingIterator pins the ownership half of the
// Load* contract: the instrumented iterator owns the one it wraps, so
// closing it closes the underlying iterator exactly once — whether the
// caller reads to the end or stops early.
//
// An early stop is the case that matters. The underlying iterator may hold a
// database cursor or an open read stream, and a caller that breaks out of
// the loop (as cqrs.NewCommandHandler's retry loop can, via its deferred
// Close) would otherwise leak it.
func TestTelemetryStore_ClosesUnderlyingIterator(t *testing.T) {
	load := map[string]func(eventsourcing.EventStore, context.Context) (*eventsourcing.Iterator[*eventsourcing.Envelope], error){
		"LoadStream": func(s eventsourcing.EventStore, ctx context.Context) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
			return s.LoadStream(ctx, "agg-1")
		},
		"LoadStreamFrom": func(s eventsourcing.EventStore, ctx context.Context) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
			return s.LoadStreamFrom(ctx, "agg-1", eventsourcing.Any{})
		},
		"LoadFromAll": func(s eventsourcing.EventStore, ctx context.Context) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
			return s.LoadFromAll(ctx, eventsourcing.Any{})
		},
	}

	for name, open := range load {
		t.Run(name, func(t *testing.T) {
			t.Run("full read", func(t *testing.T) {
				stub := &closeTrackingStore{events: envelopes(3)}
				iter, err := open(WithEventStoreTelemetry(stub), context.Background())
				if err != nil {
					t.Fatalf("%s() error = %v", name, err)
				}
				got, err := iter.All()
				if err != nil {
					t.Fatalf("All() = %v", err)
				}
				if len(got) != 3 {
					t.Fatalf("All() returned %d events, want 3", len(got))
				}
				if stub.closes != 1 {
					t.Fatalf("underlying iterator closed %d times, want 1", stub.closes)
				}
			})

			t.Run("early close", func(t *testing.T) {
				stub := &closeTrackingStore{events: envelopes(3)}
				iter, err := open(WithEventStoreTelemetry(stub), context.Background())
				if err != nil {
					t.Fatalf("%s() error = %v", name, err)
				}
				if !iter.Next() {
					t.Fatal("Next() = false on a 3-event stream")
				}
				if err := iter.Close(); err != nil {
					t.Fatalf("Close() = %v", err)
				}
				if stub.closes != 1 {
					t.Fatalf("underlying iterator closed %d times after an early Close, want 1", stub.closes)
				}
				// Idempotent: a deferred Close after an explicit one must
				// not close the underlying iterator a second time.
				if err := iter.Close(); err != nil {
					t.Fatalf("second Close() = %v", err)
				}
				if stub.closes != 1 {
					t.Fatalf("underlying iterator closed %d times after two Closes, want 1", stub.closes)
				}
			})

			t.Run("read error reaches Err", func(t *testing.T) {
				boom := errors.New("read failed")
				stub := &closeTrackingStore{events: envelopes(1), readErr: boom}
				iter, err := open(WithEventStoreTelemetry(stub), context.Background())
				if err != nil {
					t.Fatalf("%s() error = %v", name, err)
				}
				got, err := iter.All()
				if !errors.Is(err, boom) {
					t.Fatalf("All() error = %v, want %v", err, boom)
				}
				if s, want := err.Error(), boom.Error(); s != want {
					t.Fatalf("All() error = %q, want the failure reported once (%q)", s, want)
				}
				if len(got) != 1 {
					t.Fatalf("All() returned %d events, want the 1 read before the error", len(got))
				}
				if stub.closes != 1 {
					t.Fatalf("underlying iterator closed %d times, want 1", stub.closes)
				}
			})
		})
	}
}

func envelopes(n int) []*eventsourcing.Envelope {
	out := make([]*eventsourcing.Envelope, n)
	for i := range out {
		out[i] = &eventsourcing.Envelope{
			StreamID: "agg-1",
			Event:    loadFromAllStubEvent{},
			Version:  uint64(i),
		}
	}
	return out
}
