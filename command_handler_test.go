package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	loadFn func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error)
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
	return s.LoadStreamFrom(ctx, id, Any{})
}
func (s *testStore) LoadStreamFrom(ctx context.Context, id string, version StreamState) (*Iterator[*Envelope], error) {
	s.loadCalled++
	return s.loadFn(ctx, id, version)
}
func (s *testStore) LoadFromAll(ctx context.Context, version StreamState) (*Iterator[*Envelope], error) {
	return nil, nil
}
func (s *testStore) Close() error { return nil }

// ---------------------- Tests ----------------------

func TestNewCommandHandler_LoadError(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return nil, errors.New("db read failure")
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when load fails")
		return AppendResult{}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
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
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		it := NewIteratorFunc(func(ctx context.Context) (*Envelope, error) {
			return nil, errors.New("iterator fail")
		})
		return it, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s },
		func(s int, c testEvent) ([]Event, error) { return nil, nil },
	)

	_, err := handler(context.Background(), testEvent{agg: "a", typ: "t"})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected iterator error to be returned")
	}
}

func TestNewCommandHandler_DecideError_BusinessRuleViolation(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when decide returns an error")
		return AppendResult{}, nil
	}

	decideErr := errors.New("insufficient funds")
	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return nil, decideErr
		},
	)

	_, err := handler(context.Background(), testEvent{agg: "a", typ: "t"})
	if err == nil {
		t.Fatalf("expected decide's error to be returned")
	}
	var violation *ErrBusinessRuleViolation
	if !errors.As(err, &violation) {
		t.Fatalf("expected error to wrap *ErrBusinessRuleViolation, got: %v", err)
	}
	if !errors.Is(err, decideErr) {
		t.Fatalf("expected error chain to contain decide's original error, got: %v", err)
	}
}

func TestNewCommandHandler_NoEvents_NoSave(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
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
		func() int { return 0 },
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
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
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
		func() int { return 0 },
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
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		return AppendResult{Successful: false}, fmt.Errorf("disk full")
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
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
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
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
		func() int { return 0 },
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
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator([]*Envelope{prior}), nil
	}

	var seenRevision StreamState
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		seenRevision = revision
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithStreamState(Revision(5)), // explicit Revision is a hard expectation and stays fixed
	)

	_, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// seenRevision should be an StreamState equal to 7
	if seenRevision == nil {
		t.Fatalf("expected revision passed to Save to be non-nil")
	}
	switch rv := seenRevision.(type) {
	case Revision:
		if uint64(rv) != 5 {
			t.Fatalf("expected revision 5, got %d", uint64(rv))
		}
	default:
		t.Fatalf("expected StreamState, got %T", seenRevision)
	}
}

// TestNewCommandHandler_AnyRevision_RetryConverges is a regression test for
// GitHub issue #23: with the default Any{} stream state, a save conflict
// must be retried against the revision the handler just loaded, not a
// stale one, so a competing writer landing between load and save doesn't
// make every retry fail identically.
func TestNewCommandHandler_AnyRevision_RetryConverges(t *testing.T) {
	store := &testStore{}

	// First load returns the stream as it was before a competing writer's
	// append; the second load (triggered by the retry) returns the extra
	// event that competing writer landed.
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		if store.loadCalled == 1 {
			return newSliceEnvelopeIterator([]*Envelope{
				{EventID: uuid.New(), StreamID: "s", Event: testEvent{agg: "s", typ: "old"}, Version: 1, OccurredAt: time.Now()},
			}), nil
		}
		return newSliceEnvelopeIterator([]*Envelope{
			{EventID: uuid.New(), StreamID: "s", Event: testEvent{agg: "s", typ: "foreign"}, Version: 2, OccurredAt: time.Now()},
		}), nil
	}

	var seenRevisions []StreamState
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		seenRevisions = append(seenRevisions, revision)
		if len(seenRevisions) == 1 {
			return AppendResult{}, &StreamRevisionConflictError{Stream: "s", ExpectedRevision: Revision(1), ActualRevision: Revision(2)}
		}
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s + 1 },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithRetryStrategy(backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 3)),
	)

	res, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err != nil {
		t.Fatalf("expected retry to converge, got error: %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected successful append, got %+v", res)
	}

	if len(seenRevisions) != 2 {
		t.Fatalf("expected 2 save attempts, got %d", len(seenRevisions))
	}
	if rv, ok := seenRevisions[0].(Revision); !ok || uint64(rv) != 1 {
		t.Fatalf("expected first save to use Revision(1), got %#v", seenRevisions[0])
	}
	if rv, ok := seenRevisions[1].(Revision); !ok || uint64(rv) != 2 {
		t.Fatalf("expected retried save to use Revision(2) (advanced past the foreign event), got %#v", seenRevisions[1])
	}
}

// TestNewCommandHandler_ExplicitRevision_ConflictNotRetried verifies that a
// caller-supplied Revision(N) is a hard expectation: a conflict against it
// is returned immediately, even when a retry strategy is configured that
// would otherwise allow retries.
func TestNewCommandHandler_ExplicitRevision_ConflictNotRetried(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return newSliceEnvelopeIterator(nil), nil
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		return AppendResult{}, &StreamRevisionConflictError{Stream: "s", ExpectedRevision: Revision(0), ActualRevision: Revision(1)}
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithStreamState(Revision(0)),
		WithRetryStrategy(backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 5)),
	)

	_, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err == nil {
		t.Fatalf("expected the explicit Revision(0) conflict to be returned as an error")
	}
	var conflict *StreamRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected error to wrap *StreamRevisionConflictError, got: %v", err)
	}
	if store.saveCalled != 1 {
		t.Fatalf("expected exactly 1 save attempt (no retry for an explicit Revision), got %d", store.saveCalled)
	}
}

// TestNewCommandHandler_StreamExists_MissingStreamFailsFast verifies that a
// StreamExists{} load failure (the stream doesn't exist yet) is returned
// immediately, never retried — even with a retry strategy configured. Only
// once a StreamExists load succeeds does the handler stop pinning an exact
// expectation and start auto-converging on save conflicts (see
// TestNewCommandHandler_StreamExists_ConflictRetries).
func TestNewCommandHandler_StreamExists_MissingStreamFailsFast(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		return nil, fmt.Errorf("stream %q: should exist: %w", stream, ErrStreamNotFound)
	}
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when the stream doesn't exist")
		return AppendResult{}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s + 1 },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithStreamState(StreamExists{}),
		WithRetryStrategy(backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 3)),
	)

	_, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err == nil {
		t.Fatalf("expected an error when the stream doesn't exist")
	}
	if !errors.Is(err, ErrStreamNotFound) {
		t.Fatalf("expected error to wrap ErrStreamNotFound, got: %v", err)
	}
	if store.loadCalled != 1 {
		t.Fatalf("expected exactly 1 load attempt (no retry while the stream doesn't exist), got %d", store.loadCalled)
	}
}

// TestNewCommandHandler_StreamExists_ConflictRetries verifies that once the
// stream is confirmed to exist, StreamExists{} behaves like Any{} for
// concurrency conflicts: it isn't pinned to a version, so a conflict is
// resolved by reloading and retrying rather than failing immediately.
func TestNewCommandHandler_StreamExists_ConflictRetries(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		if store.loadCalled == 1 {
			return newSliceEnvelopeIterator([]*Envelope{
				{EventID: uuid.New(), StreamID: "s", Event: testEvent{agg: "s", typ: "old"}, Version: 1, OccurredAt: time.Now()},
			}), nil
		}
		return newSliceEnvelopeIterator([]*Envelope{
			{EventID: uuid.New(), StreamID: "s", Event: testEvent{agg: "s", typ: "foreign"}, Version: 2, OccurredAt: time.Now()},
		}), nil
	}

	var seenRevisions []StreamState
	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		seenRevisions = append(seenRevisions, revision)
		if len(seenRevisions) == 1 {
			return AppendResult{}, &StreamRevisionConflictError{Stream: "s", ExpectedRevision: Revision(1), ActualRevision: Revision(2)}
		}
		return AppendResult{Successful: true, NextExpectedVersion: envelopes[len(envelopes)-1].Version}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s + 1 },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "e"}}, nil
		},
		WithStreamState(StreamExists{}),
		WithRetryStrategy(backoff.WithMaxRetries(backoff.NewConstantBackOff(time.Millisecond), 3)),
	)

	res, err := handler(context.Background(), testEvent{agg: "s", typ: "c"})
	if err != nil {
		t.Fatalf("expected retry to converge, got error: %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected successful append, got %+v", res)
	}
	if len(seenRevisions) != 2 {
		t.Fatalf("expected 2 save attempts, got %d", len(seenRevisions))
	}
	if rv, ok := seenRevisions[1].(Revision); !ok || uint64(rv) != 2 {
		t.Fatalf("expected retried save to use Revision(2) (advanced past the foreign event), got %#v", seenRevisions[1])
	}
}

func TestNewCommandHandler_MetadataMergeOrder(t *testing.T) {
	store := &testStore{}
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
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
		func() int { return 0 },
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

func TestNewCommandHandler_UnregisteredEventError(t *testing.T) {
	// This test verifies that when an iterator encounters an unregistered event
	// (e.g., from NewEventByName failing), the error is properly propagated
	// through the command handler.

	store := &testStore{}

	// Simulate an iterator that returns one event successfully, then fails
	// on the second event because it's not registered
	callCount := 0
	store.loadFn = func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
		callCount = 0 // reset for each load call
		iter := NewIteratorFunc(func(ctx context.Context) (*Envelope, error) {
			callCount++
			if callCount == 1 {
				// First event succeeds
				return &Envelope{
					EventID:  uuid.New(),
					StreamID: stream,
					Event:    testEvent{agg: stream, typ: "known", val: "v1"},
					Version:  1,
				}, nil
			}
			// Second event fails - simulating an unregistered event error
			// This is the error that would come from NewEventByName in KurrentDB
			return nil, fmt.Errorf("cannot create event %q: %w", "UnknownEvent", ErrEventNotRegistered)
		})
		return iter, nil
	}

	store.saveFn = func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
		t.Fatalf("Save should not be called when iterator fails")
		return AppendResult{}, nil
	}

	handler := NewCommandHandler(
		store,
		func() int { return 0 },
		func(s int, e *Envelope) int { return s + 1 },
		func(s int, cmd testEvent) ([]Event, error) {
			return []Event{testEvent{agg: cmd.AggregateID(), typ: "new"}}, nil
		},
		WithRetryStrategy(&backoff.StopBackOff{}),
	)

	_, err := handler(context.Background(), testEvent{agg: "test-stream", typ: "cmd"})

	// Verify the error is propagated
	if err == nil {
		t.Fatalf("expected error when iterator encounters unregistered event")
	}

	// Verify the error contains the expected information
	if !errors.Is(err, ErrEventNotRegistered) {
		t.Fatalf("expected error to wrap ErrEventNotRegistered, got: %v", err)
	}

	// Verify the error message contains context about the failed event
	errStr := err.Error()
	if !strings.Contains(errStr, "UnknownEvent") {
		t.Fatalf("expected error message to contain event name 'UnknownEvent', got: %v", errStr)
	}
	if !strings.Contains(errStr, "iter failed") {
		t.Fatalf("expected error message to contain 'iter failed', got: %v", errStr)
	}
}
