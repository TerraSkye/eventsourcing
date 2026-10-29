package telemetry

import (
	"context"
	"testing"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
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

	if err := handler.Handle(context.Background(), stubEvent{}); err != nil {
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

// TestWithEventTelemetry_LinksOnlyToAProducer covers the producer link: an
// event carrying a trace context is linked to it, and one without gets no
// link, rather than an empty one.
func TestWithEventTelemetry_LinksOnlyToAProducer(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	handler := WithEventTelemetry(eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		return nil
	}))
	producer := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1},
		SpanID:     trace.SpanID{2},
		TraceFlags: trace.FlagsSampled,
	})
	tests := []struct {
		name     string
		metadata map[string]any
		want     int
	}{
		{"without a trace", nil, 0},
		{"with a trace", map[string]any{"traceparent": "00-" + producer.TraceID().String() + "-" + producer.SpanID().String() + "-01"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := eventsourcing.WithEnvelope(context.Background(), &eventsourcing.Envelope{StreamID: "agg-1", Event: stubEvent{}, Metadata: tt.metadata})
			before := spanNameRecorder.len()
			if err := handler.Handle(ctx, stubEvent{}); err != nil {
				t.Fatalf("Handle() error = %v", err)
			}
			spans := spanNameRecorder.spansSince(before)
			if len(spans) != 1 {
				t.Fatalf("started %d spans, want 1", len(spans))
			}
			if got := len(spans[0].links); got != tt.want {
				t.Fatalf("span has %d links, want %d: %v", got, tt.want, spans[0].links)
			}
			if tt.want == 1 && spans[0].links[0].SpanContext.SpanID() != producer.SpanID() {
				t.Errorf("link to span %s, want the producer's %s", spans[0].links[0].SpanContext.SpanID(), producer.SpanID())
			}
		})
	}
}

// TestWithEventTelemetry_SkippedResult covers a handler that declined the
// event: recorded with result "skipped", apart from a success or a failure.
func TestWithEventTelemetry_SkippedResult(t *testing.T) {
	const instrument = "eventsourcing.event_handler.duration"
	handler := WithEventTelemetry(eventsourcing.NewEventHandlerFunc(func(_ context.Context, event eventsourcing.Event) error {
		return &eventsourcing.SkippedEventError{Event: event}
	}))

	from := len(durationRecorder.recordsFor(instrument))
	if err := handler.Handle(context.Background(), stubEvent{}); err == nil {
		t.Fatal("expected the skip to be returned")
	}
	got := durationRecorder.recordsFor(instrument)[from:]
	if len(got) != 1 {
		t.Fatalf("recorded %d %s values, want 1", len(got), instrument)
	}
	if r := attrString(got[0].attrs, AttrResult); r != "skipped" {
		t.Errorf("%s = %q, want skipped", AttrResult, r)
	}
}
