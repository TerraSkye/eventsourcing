package eventsourcing

import (
	"context"
	"testing"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

// chainHandler returns a handler over an empty stream whose decide yields
// two events, and the envelopes its last call saved.
func chainHandler(t *testing.T) (CommandHandler[testEvent], *[]Envelope) {
	t.Helper()
	var saved []Envelope
	store := &testStore{
		loadFn: func(ctx context.Context, stream string, from StreamState) (*Iterator[*Envelope], error) {
			return newSliceEnvelopeIterator(ctx, nil), nil
		},
		saveFn: func(ctx context.Context, envelopes []Envelope, revision StreamState) (AppendResult, error) {
			saved = envelopes
			return AppendResult{Successful: true}, nil
		},
	}
	decide := func(state int, cmd testEvent) ([]Event, error) {
		return []Event{
			testEvent{agg: cmd.AggregateID(), typ: "e1"},
			testEvent{agg: cmd.AggregateID(), typ: "e2"},
		}, nil
	}
	h := NewCommandHandler(store, func() int { return 0 }, func(s int, e *Envelope) int { return s }, decide,
		WithRetryStrategy(&backoff.StopBackOff{}))
	return h, &saved
}

// TestNewCommandHandler_StartsAChain covers a command no event caused, such
// as one from an API request: its events start a chain named after the
// first of them, with no causation, and record the command in metadata.
func TestNewCommandHandler_StartsAChain(t *testing.T) {
	h, saved := chainHandler(t)
	if _, err := h(t.Context(), testEvent{agg: "a-1"}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	evs := *saved
	root := evs[0].EventID.String()
	for i, e := range evs {
		if e.CorrelationID != root {
			t.Errorf("event %d CorrelationID = %q, want the first event's ID %q", i, e.CorrelationID, root)
		}
		if e.CausationID != "" {
			t.Errorf("event %d CausationID = %q, want none for the root of a chain", i, e.CausationID)
		}
		if e.Metadata["command_type"] != "testEvent" {
			t.Errorf("event %d command_type = %v, want testEvent", i, e.Metadata["command_type"])
		}
	}
	if id, _ := evs[0].Metadata["command_id"].(string); id == "" || id != evs[1].Metadata["command_id"] {
		t.Errorf("command_id = %v, %v; want one ID shared by the call's events", evs[0].Metadata["command_id"], evs[1].Metadata["command_id"])
	}
}

// TestNewCommandHandler_JoinsTheChainOfTheHandledEvent covers a command sent
// while handling an event, as an event bus or the projection runner
// delivers it: the resulting events share that event's correlation and are
// caused by it.
func TestNewCommandHandler_JoinsTheChainOfTheHandledEvent(t *testing.T) {
	h, saved := chainHandler(t)
	cause := &Envelope{EventID: uuid.New(), Event: testEvent{agg: "a-1"}, CorrelationID: "chain-1", CausationID: uuid.NewString()}
	if _, err := h(WithEnvelope(t.Context(), cause), testEvent{agg: "a-2"}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	for i, e := range *saved {
		if e.CorrelationID != "chain-1" {
			t.Errorf("event %d CorrelationID = %q, want the handled event's %q", i, e.CorrelationID, "chain-1")
		}
		if e.CausationID != cause.EventID.String() {
			t.Errorf("event %d CausationID = %q, want the handled event's ID %q", i, e.CausationID, cause.EventID)
		}
	}
}

// TestNewCommandHandler_CorrelationFromRequest covers a chain started on
// purpose, with the API layer's request ID.
func TestNewCommandHandler_CorrelationFromRequest(t *testing.T) {
	h, saved := chainHandler(t)
	if _, err := h(WithCorrelationID(t.Context(), "request-7"), testEvent{agg: "a-1"}); err != nil {
		t.Fatalf("handle: %v", err)
	}
	for i, e := range *saved {
		if e.CorrelationID != "request-7" || e.CausationID != "" {
			t.Errorf("event %d chain = %q/%q, want request-7 and no causation", i, e.CorrelationID, e.CausationID)
		}
	}
}
