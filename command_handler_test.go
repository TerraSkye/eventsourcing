package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

// ---------------------- Test helpers / stubs ----------------------

// testEvent implements your Event interface.
type testEvent struct {
	agg string
	typ string
	val string
}

func (e testEvent) AggregateID() string { return e.agg }
func (e testEvent) EventType() string   { return e.typ }

// testIterator wraps the Iterator[*Envelope] constructor helpers.
func newSliceEnvelopeIterator(envs []*Envelope) *Iterator[*Envelope] {
	return NewSliceIterator(envs)
}

type testStore struct {
	// configurable behavior
	loadFn func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error)
	saveFn func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error)

	// tracking
	loadCalled int
	saveCalled int
}

func (s *testStore) Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error) {
	s.saveCalled++
	return s.saveFn(ctx, events, revision)
}
func (s *testStore) LoadStream(ctx context.Context, id string) (*Iterator[*Envelope], error) {
	return s.LoadStreamFrom(ctx, id, 0)
}
func (s *testStore) LoadStreamFrom(ctx context.Context, id string, version uint64) (*Iterator[*Envelope], error) {
	s.loadCalled++
	return s.loadFn(ctx, id, version)
}
func (s *testStore) LoadFromAll(ctx context.Context, version uint64) (*Iterator[*Envelope], error) {
	return nil, nil
}
func (s *testStore) Close() error { return nil }

// ---------------------- Tests ----------------------

func TestNewCommandHandler_LoadError(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return nil, errors.New("db read failure")
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when load fails")
		return AppendResult{}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, c testEvent) ([]Event, error) { return nil, nil },
		WithRetryStrategy(&backoff.StopBackOff{}),
	)

	_, err := handler(context.Background(), testEvent{agg: "a", typ: "t"})
	if err == nil {
		t.Fatalf("expected error when LoadStreamFrom fails")
	}
	if err.Error() == "" {
		t.Fatalf("expected non-empty error")
	}
	if store.loadCalled != 1 {
		t.Fatalf("expected load called once, got %d", store.loadCalled)
	}
}

func TestNewCommandHandler_IteratorErr(t *testing.T) {
	store := &testStore{}

	// produce an iterator that returns an error on Next
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		it := NewIteratorFunc(func(ctx context.Context) (*Envelope, error) {
			return nil, errors.New("iterator fail")
		})
		return it, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, c testEvent) ([]Event, error) { return nil, nil },
	)

	_, err := handler(context.Background(), testEvent{agg: "a", typ: "t"})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected iterator error to be returned")
	}
}

func TestNewCommandHandler_NoEvents_NoSave(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		// no prior events
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when decide returns no events")
		return AppendResult{}, nil
	}

	decide := func(state int, cmd testEvent) ([]Event, error) {
		return []Event{}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		decide,
	)

	res, err := handler(context.Background(), testEvent{agg: "agg1", typ: "t"})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected Successful true when no events produced")
	}
	// NextExpectedVersion should be 0 (no prior events)
	if res.NextExpectedVersion != 0 {
		t.Fatalf("expected NextExpectedVersion 0, got %d", res.NextExpectedVersion)
	}
	if store.loadCalled != 1 {
		t.Fatalf("expected load called once, got %d", store.loadCalled)
	}
}

func TestNewCommandHandler_SaveSuccess_Versioning_Metadata_StreamName(t *testing.T) {
	store := &testStore{}

	// Simulate one prior event version=1
	prior := &Envelope{
		EventID:    uuid.New(),
		StreamID:   "agg-1",
		Event:      testEvent{agg: "agg-1", typ: "old", val: "v"},
		Version:    1,
		OccurredAt: time.Now(),
	}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator([]*Envelope{prior}), nil
	}

	// Check payload saved
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		// Expect two events saved
		if len(envelopes) != 2 {
			t.Fatalf("expected 2 envelopes, got %d", len(envelopes))
		}
		// versions should be 2 and 3
		if envelopes[0].Version != 2 || envelopes[1].Version != 3 {
			t.Fatalf("expected versions [2,3], got [%d,%d]", envelopes[0].Version, envelopes[1].Version)
		}
		// metadata should contain merged keys; m should be "x" (from extractor)
		if envelopes[0].Metadata["m"] != "x" {
			t.Fatalf("expected metadata m=x, got %v", envelopes[0].Metadata)
		}
		// stream name should be as provided by custom StreamNamer
		if envelopes[0].StreamID != "stream-"+envelopes[0].Event.AggregateID() {
			t.Fatalf("unexpected stream name: %s", envelopes[0].StreamID)
		}
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	decide := func(state int, cmd testEvent) ([]Event, error) {
		return []Event{
			testEvent{agg: cmd.AggregateID(), typ: "e1", val: "a"},
			testEvent{agg: cmd.AggregateID(), typ: "e2", val: "b"},
		}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s + 1 }, // evolve increments state
		decide,
		WithMetadataExtractor(func(ctx context.Context) map[string]any {
			return map[string]any{"m": "x"}
		}),
		WithStreamNamer(func(ctx context.Context, cmd Command) string {
			return "stream-" + cmd.AggregateID()
		}),
		WithRetryStrategy(&backoff.StopBackOff{}),
	)

	res, err := handler(context.Background(), testEvent{agg: "agg-1", typ: "cmd"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected success")
	}
	if res.NextExpectedVersion != 3 {
		t.Fatalf("expected next expected version 3, got %d", res.NextExpectedVersion)
	}
	if store.saveCalled != 1 {
		t.Fatalf("expected save called once, got %d", store.saveCalled)
	}
}

func TestNewCommandHandler_SavePermanentError(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		return AppendResult{Successful: false}, fmt.Errorf("disk full")
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: "a", typ: "e", val: "v"}}, nil
		},
		WithRetryStrategy(&backoff.StopBackOff{}),
	)

	_, err := handler(context.Background(), testEvent{agg: "a", typ: "cmd"})
	if err == nil {
		t.Fatalf("expected error when save returns generic error")
	}
	if err.Error() == "" {
		t.Fatalf("expected non-empty error message")
	}
}

func TestNewCommandHandler_SaveConflict_Retry(t *testing.T) {
	store := &testStore{}
	// no prior events
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}

	callCount := 0
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		callCount++
		if callCount == 1 {
			// return a StreamRevisionConflictError to trigger retry
			// assume the concrete type exists in package (NewCommandHandler checks via errors.As)
			return AppendResult{Successful: false}, &StreamRevisionConflictError{}
		}
		// second call succeed
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e", val: "v"}}, nil
		},
		// use a retry backoff that allows at least one retry
		WithRetryStrategy(backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Millisecond), 3)),
	)

	res, err := handler(context.Background(), testEvent{agg: "agg", typ: "c"})
	if err != nil {
		t.Fatalf("unexpected error from handler with retry: %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected success after retry")
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 save attempts, got %d", callCount)
	}
}

func TestNewCommandHandler_ExplicitRevision_Update(t *testing.T) {
	store := &testStore{}
	// simulate prior events up to version 7
	prior := &Envelope{
		EventID:    uuid.New(),
		StreamID:   "s",
		Event:      testEvent{agg: "s", typ: "old"},
		Version:    7,
		OccurredAt: time.Now(),
	}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator([]*Envelope{prior}), nil
	}

	var seenRevision StreamState
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		seenRevision = revision
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithRevision(Revision(5)), // will be updated to latest loaded revision (7)
	)

	_, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// seenRevision should be an Revision equal to 7
	if seenRevision == nil {
		t.Fatalf("expected revision passed to Save to be non-nil")
	}
	switch rv := seenRevision.(type) {
	case Revision:
		if uint64(rv) != 7 {
			t.Fatalf("expected revision 7, got %d", uint64(rv))
		}
	default:
		t.Fatalf("expected Revision, got %T", seenRevision)
	}
}

func TestNewCommandHandler_MetadataMergeOrder(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from uint64) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		// verify metadata merged and overwritten by later extractor
		if envelopes[0].Metadata["k"] != "b" {
			t.Fatalf("expected metadata key 'k' to be overwritten by later extractor; got %v", envelopes[0].Metadata)
		}
		if envelopes[0].Metadata["first_only"] != "1" {
			t.Fatalf("expected first_only key present")
		}
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[0].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		0,
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithMetadataExtractor(func(ctx context.Context) map[string]any {
			return map[string]any{"k": "a", "first_only": "1"}
		}),
		WithMetadataExtractor(func(ctx context.Context) map[string]any {
			return map[string]any{"k": "b"}
		}),
	)

	_, err := handler(context.Background(), testEvent{agg: "m", typ: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
