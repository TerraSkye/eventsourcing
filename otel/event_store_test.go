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
	stub := &stubStore{}
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
					Event:    stubEvent{},
					Version:  uint64(n),
					Metadata: shared,
				},
			}
			_, _ = store.Save(ctx, events, eventsourcing.Any{})
		}(i)
	}
	wg.Wait()
}

type stubEvent struct{}

func (stubEvent) AggregateID() string { return "agg-1" }
func (stubEvent) EventType() string   { return "stub.event" }

// stubStore is a minimal EventStore and GlobalReader over a fixed slice of
// events.
type stubStore struct {
	events  []*eventsourcing.Envelope
	readErr error
}

func (s *stubStore) Save(context.Context, []eventsourcing.Envelope, eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	return eventsourcing.AppendResult{}, nil
}

func (s *stubStore) LoadStream(ctx context.Context, _ string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(ctx, s.events), nil
}

func (s *stubStore) LoadStreamFrom(ctx context.Context, _ string, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator(ctx, s.events), nil
}

func (s *stubStore) ReadAll(_ context.Context, after uint64, _ eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	if s.readErr != nil {
		return eventsourcing.ReadResult{}, s.readErr
	}
	res := eventsourcing.ReadResult{Next: after, AtEnd: true}
	for _, e := range s.events {
		if e.GlobalVersion > after {
			res.Events = append(res.Events, e)
			res.Next = e.GlobalVersion
		}
	}
	return res, nil
}

func (s *stubStore) Head(context.Context) (uint64, error) {
	if len(s.events) == 0 {
		return 0, nil
	}
	return s.events[len(s.events)-1].GlobalVersion, nil
}

func (s *stubStore) Close() error { return nil }

// TestTelemetryStore_ReadAll forwards ReadAll and Head to a wrapped
// GlobalReader unchanged, passes its errors through, and does not implement
// GlobalReader itself for a store that cannot read the global log.
func TestTelemetryStore_ReadAll(t *testing.T) {
	ctx := context.Background()
	stub := &stubStore{}
	for i := 1; i <= 3; i++ {
		stub.events = append(stub.events, &eventsourcing.Envelope{
			StreamID: "agg-1", Event: stubEvent{}, Version: uint64(i), GlobalVersion: uint64(i),
		})
	}
	reader, ok := WithEventStoreTelemetry(stub).(eventsourcing.GlobalReader)
	if !ok {
		t.Fatal("WithEventStoreTelemetry(GlobalReader) does not implement eventsourcing.GlobalReader")
	}

	t.Run("forwards", func(t *testing.T) {
		res, err := reader.ReadAll(ctx, 1, eventsourcing.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		if len(res.Events) != 2 || res.Next != 3 || !res.AtEnd {
			t.Fatalf("ReadAll(after 1) = %d events, Next %d, AtEnd %v; want 2, 3, true", len(res.Events), res.Next, res.AtEnd)
		}
		head, err := reader.Head(ctx)
		if err != nil || head != 3 {
			t.Fatalf("Head() = %d, %v; want 3, nil", head, err)
		}
	})

	t.Run("error passes through", func(t *testing.T) {
		boom := errors.New("read failed")
		failing, ok := WithEventStoreTelemetry(&stubStore{readErr: boom}).(eventsourcing.GlobalReader)
		if !ok {
			t.Fatal("WithEventStoreTelemetry(GlobalReader) does not implement eventsourcing.GlobalReader")
		}
		if _, err := failing.ReadAll(ctx, 0, eventsourcing.ReadOptions{}); !errors.Is(err, boom) {
			t.Fatalf("ReadAll() error = %v, want %v", err, boom)
		}
	})

	t.Run("unsupported store", func(t *testing.T) {
		plain := WithEventStoreTelemetry(&closeTrackingStore{})
		if _, ok := plain.(eventsourcing.GlobalReader); ok {
			t.Errorf("WithEventStoreTelemetry(%T) implements eventsourcing.GlobalReader, want only eventsourcing.EventStore", &closeTrackingStore{})
		}
	})
}

// TestWithEventStoreTelemetry_WithOperationIgnoredForSaveSpan verifies
// WithEventStoreTelemetry's doc comment promise that "Options such as
// [WithAttributes] and [WithOperation] customize the spans produced" holds
// for TelemetryStore.Save's span, not just its hardcoded default name
// "append eventstore".
func TestWithEventStoreTelemetry_WithOperationIgnoredForSaveSpan(t *testing.T) {
	const wantSpanName = "custom-save-operation"

	stub := &stubStore{}
	store := WithEventStoreTelemetry(stub, WithOperation(wantSpanName))

	before := spanNameRecorder.len()

	_, err := store.Save(context.Background(), []eventsourcing.Envelope{
		{StreamID: "agg-1", Event: stubEvent{}, Version: 1},
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
			Event:    stubEvent{},
			Version:  uint64(i),
		}
	}
	return out
}
