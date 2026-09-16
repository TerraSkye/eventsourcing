package logging

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
)

type testEvent struct {
	OrderID string
}

func (e *testEvent) AggregateID() string { return e.OrderID }
func (e *testEvent) EventType() string   { return "testEvent" }

// envelopeContext returns a context carrying the envelope fields the event
// logging middleware reads, so the tests can assert they reach the log record.
func envelopeContext(eventID uuid.UUID) context.Context {
	ctx := cqrs.WithEnvelope(context.Background(), &cqrs.Envelope{
		EventID:       eventID,
		StreamID:      "order-1",
		Event:         &testEvent{OrderID: "order-1"},
		Version:       4,
		GlobalVersion: 91,
	})
	return cqrs.WithCausation(ctx, "logging.testCommand")
}

// TestWithLoggingMiddleware_LogsSuccessWithDuration asserts the completion
// record carries how long handling took, alongside the envelope fields.
func TestWithLoggingMiddleware_LogsSuccessWithDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	const handlerDelay = 2 * time.Millisecond
	handler := WithLoggingMiddleware(logger, cqrs.NewEventHandlerFunc(
		func(ctx context.Context, event cqrs.Event) error {
			time.Sleep(handlerDelay)
			return nil
		},
	))

	eventID := uuid.New()
	if err := handler.Handle(envelopeContext(eventID), &testEvent{OrderID: "order-1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2 (one before the call, one after): %v", len(recs), recs)
	}

	if recs[0]["msg"] != "event processing started" {
		t.Errorf("first msg = %v, want %q", recs[0]["msg"], "event processing started")
	}

	done := recs[1]
	if done["level"] != "DEBUG" {
		t.Errorf("completion level = %v, want DEBUG", done["level"])
	}
	if done["msg"] != "event processed successfully" {
		t.Errorf("completion msg = %v, want %q", done["msg"], "event processed successfully")
	}
	if done["event"] != "testEvent" {
		t.Errorf("event = %v, want %q", done["event"], "testEvent")
	}
	if done["stream-id"] != "order-1" {
		t.Errorf("stream-id = %v, want %q", done["stream-id"], "order-1")
	}
	if done["aggregateId"] != "order-1" {
		t.Errorf("aggregateId = %v, want %q", done["aggregateId"], "order-1")
	}
	if done["causation"] != "logging.testCommand" {
		t.Errorf("causation = %v, want %q", done["causation"], "logging.testCommand")
	}
	if done["version"] != float64(4) {
		t.Errorf("version = %v, want 4", done["version"])
	}
	if done["global-version"] != float64(91) {
		t.Errorf("global-version = %v, want 91", done["global-version"])
	}
	if done["event-id"] != eventID.String() {
		t.Errorf("event-id = %v, want %q", done["event-id"], eventID)
	}
	if d := requireDuration(t, done); d < handlerDelay {
		t.Errorf("duration = %v, want at least the %v the handler slept", d, handlerDelay)
	}
}

// TestWithLoggingMiddleware_SkippedEventIsNotAnError asserts that a handler
// declining an event type it does not want is logged as the intentional skip
// it is, matching the otel package, which marks such a span Ok rather than
// failed. Without this, a bus fanning every event out to every subscriber
// fills the error log with routine misses.
func TestWithLoggingMiddleware_SkippedEventIsNotAnError(t *testing.T) {
	logger, buf := newCapturingLogger()

	event := &testEvent{OrderID: "order-1"}
	handler := WithLoggingMiddleware(logger, cqrs.NewEventHandlerFunc(
		func(ctx context.Context, ev cqrs.Event) error {
			return &cqrs.ErrSkippedEvent{Event: ev}
		},
	))

	err := handler.Handle(envelopeContext(uuid.New()), event)
	var skipped *cqrs.ErrSkippedEvent
	if !errors.As(err, &skipped) {
		t.Fatalf("Handle returned %v, want the skip unchanged", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "DEBUG" {
		t.Errorf("level = %v, want DEBUG — a skipped event is not a failure", done["level"])
	}
	if done["msg"] != "event skipped" {
		t.Errorf("msg = %v, want %q", done["msg"], "event skipped")
	}
	requireDuration(t, done)

	for _, rec := range recs {
		if rec["level"] == "ERROR" {
			t.Errorf("logged an ERROR record for a skipped event: %v", rec)
		}
	}
}

// TestWithLoggingMiddleware_ErrorLogsDuration asserts a genuine handler
// failure is still logged at error level, now with the duration alongside it.
func TestWithLoggingMiddleware_ErrorLogsDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	wantErr := errors.New("projection write failed")
	handler := WithLoggingMiddleware(logger, cqrs.NewEventHandlerFunc(
		func(ctx context.Context, ev cqrs.Event) error {
			return wantErr
		},
	))

	if err := handler.Handle(envelopeContext(uuid.New()), &testEvent{OrderID: "order-1"}); !errors.Is(err, wantErr) {
		t.Fatalf("Handle returned %v, want %v", err, wantErr)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", done["level"])
	}
	if done["msg"] != "error processing event" {
		t.Errorf("msg = %v, want %q", done["msg"], "error processing event")
	}
	if done["error"] != wantErr.Error() {
		t.Errorf("error = %v, want %q", done["error"], wantErr.Error())
	}
	requireDuration(t, done)
}

// TestEventLogging_MiddlewareLogsCompletion asserts the bus-wide middleware
// form logs the same completion record as the wrapper it delegates to.
func TestEventLogging_MiddlewareLogsCompletion(t *testing.T) {
	logger, buf := newCapturingLogger()

	middleware := EventLogging(logger)
	handler := middleware(cqrs.NewEventHandlerFunc(
		func(ctx context.Context, ev cqrs.Event) error { return nil },
	))

	if err := handler.Handle(envelopeContext(uuid.New()), &testEvent{OrderID: "order-1"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}
	if recs[1]["msg"] != "event processed successfully" {
		t.Errorf("completion msg = %v, want %q", recs[1]["msg"], "event processed successfully")
	}
	requireDuration(t, recs[1])
}
