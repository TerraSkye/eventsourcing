package otel

import (
	"context"
	"testing"

	"github.com/terraskye/eventsourcing"
)

// TestWithEventTelemetry_WithOperationIgnored verifies that
// WithEventTelemetry honors the same [Option] values as its siblings
// WithCommandTelemetry and WithQueryTelemetry — including [WithOperation]
// and [WithOperationGetter] — to name the span it starts, rather than
// hardcoding it to the literal "process event".
func TestWithEventTelemetry_WithOperationIgnored(t *testing.T) {
	const wantSpanName = "custom-process-event-operation"

	next := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	})
	handler := WithEventTelemetry(next, WithOperation(wantSpanName))

	before := spanNameRecorder.len()

	if err := handler.Handle(context.Background(), loadFromAllStubEvent{}); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}

	got := spanNameRecorder.since(before)
	if len(got) == 0 {
		t.Fatalf("no span was started for Handle()")
	}

	found := false
	for _, name := range got {
		if name == wantSpanName {
			found = true
		}
	}
	if !found {
		t.Fatalf("WithOperation(%q) had no effect; span(s) started: %v (want a span named %q)", wantSpanName, got, wantSpanName)
	}
}
