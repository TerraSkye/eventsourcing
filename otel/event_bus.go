package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var _ eventsourcing.EventBus = (*TelemetryEventBus)(nil)

// TelemetryEventBus wraps an EventBus with OpenTelemetry tracing and metrics.
//
// This decorator intercepts event subscriptions to automatically instrument
// event handlers with distributed tracing and metrics collection. It creates
// a two-level span hierarchy for each event: an outer consumer span for the
// subscription and an inner span for the actual event handling logic.
//
// The wrapper maintains trace context propagation by extracting trace information
// from event metadata and linking it to the consumer span, enabling end-to-end
// distributed tracing from command execution through event processing.
type TelemetryEventBus struct {
	next eventsourcing.EventBus
	cfg  *config
}

// Subscribe registers an event handler wrapped with telemetry instrumentation.
//
// The subscription wrapper performs the following steps for each received event:
//  1. Extracts trace context from event metadata to establish distributed trace links.
//  2. Creates an outer consumer span named "subscription.receive {name}" with event attributes.
//  3. Links the consumer span to the original producer trace for correlation.
//  4. Increments the EventBusHandled metric.
//  5. Creates an inner span named "events.handle {eventType}" for the handler execution.
//  6. Invokes the underlying event handler.
//  7. Records the EventBusDuration metric with processing time.
//  8. Updates span status based on handler result:
//     - ErrSkippedEvent: span status OK (intentional skip)
//     - Other errors: span status Error, increments EventBusErrors metric
//     - Success: span status OK
//
// Parameters:
//   - ctx: Context for the subscription setup.
//   - name: Unique identifier for this subscription, used in span names and attributes.
//   - next: The event handler to be wrapped with telemetry instrumentation.
//   - options: Optional subscriber configuration options passed to the underlying bus.
//
// Returns:
//   - An error if the subscription registration fails.
//
// Behavior Details:
//   - The outer span uses SpanKindConsumer to indicate event consumption.
//   - The inner span uses SpanKindInternal for the handler logic.
//   - Trace links connect the consumer span to the original command/producer trace.
//   - Metrics recorded:
//   - EventBusHandled: count of events received by this subscription.
//   - EventBusDuration: handler execution time in milliseconds.
//   - EventBusErrors: count of handler errors (excludes ErrSkippedEvent).
//   - Span attributes include event type, event ID, global position, stream position,
//     stream ID, and subscriber name.
//
// Example Usage:
//
//	bus := otel.WithEventBusTelemetry(eventBus)
//	err := bus.Subscribe(ctx, "order-projector", orderProjector)
func (t *TelemetryEventBus) Subscribe(ctx context.Context, name string, next eventsourcing.EventHandler, options ...eventsourcing.SubscriberOption) error {

	handler := eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {

		attr := []attribute.KeyValue{
			AttrEventType.String(event.EventType()),
			AttrEventID.String(eventsourcing.EventIDFromContext(ctx).String()),
			AttrEventGlobalPos.String(fmt.Sprintf("%d", eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.String(fmt.Sprintf("%d", eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
			AttrSubscriberName.String(name),
		}

		operationName := fmt.Sprintf("events.handle %s", event.EventType())

		var span trace.Span
		ctx, span = tracer.Start(ctx, operationName,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		return next.Handle(ctx, event)
	})

	return t.next.Subscribe(ctx, name, eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		// Extract the original trace context from event metadata
		var carrier = make(propagation.MapCarrier)
		if metadata := eventsourcing.MetadataFromContext(ctx); len(metadata) > 0 {
			for k, v := range metadata {
				if stringV, ok := v.(string); ok && len(stringV) > 0 {
					carrier[k] = stringV
				}
			}
		}

		attr := []attribute.KeyValue{
			AttrEventType.String(event.EventType()),
			AttrEventID.String(eventsourcing.EventIDFromContext(ctx).String()),
			AttrEventGlobalPos.String(fmt.Sprintf("%d", eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.String(fmt.Sprintf("%d", eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
			AttrSubscriberName.String(name),
		}

		attr = append(attr, t.cfg.Attributes...)

		if t.cfg.GetAttributes != nil {
			attr = append(attr, t.cfg.GetAttributes(ctx)...)
		}

		// Extract the SpanContext from the original trace
		originalCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		originalSpanContext := trace.SpanContextFromContext(originalCtx)

		operationName := fmt.Sprintf("subscription.receive %s", name)

		ctx, span := tracer.Start(ctx, operationName,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithLinks(trace.Link{
				SpanContext: originalSpanContext,
				Attributes: []attribute.KeyValue{
					attribute.String("link.reason", "event.consumed.from.stream"),
				},
			}),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		EventBusHandled.Add(ctx, 1, metric.WithAttributes(AttrEventType.String(event.EventType())))

		startTime := time.Now()
		err := handler.Handle(ctx, event)
		EventBusDuration.Record(ctx,
			float64(time.Since(startTime).Milliseconds()),
			metric.WithAttributes(AttrEventType.String(event.EventType())),
		)

		if err != nil {
			var skipped *eventsourcing.ErrSkippedEvent
			if errors.As(err, &skipped) {
				span.SetStatus(codes.Ok, "")
			} else {
				EventBusErrors.Add(ctx, 1, metric.WithAttributes(AttrEventType.String(event.EventType())))
				span.SetStatus(codes.Error, err.Error())
				span.RecordError(err)
			}
			return err
		}
		span.SetStatus(codes.Ok, "")
		return nil

	}), options...)

}

// Errors returns the error channel from the underlying event bus.
//
// This method delegates directly to the wrapped EventBus without additional
// instrumentation, as errors are already tracked at the handler level.
func (t *TelemetryEventBus) Errors() <-chan error {
	return t.next.Errors()
}

// Close closes the underlying event bus and waits for all handlers to finish.
//
// This method delegates directly to the wrapped EventBus without additional
// instrumentation.
func (t *TelemetryEventBus) Close() error {
	return t.next.Close()
}

// WithEventBusTelemetry wraps an EventBus with OpenTelemetry tracing and metrics.
//
// This constructor creates a TelemetryEventBus that decorates all subscriptions
// with automatic tracing spans and metrics collection. The wrapper preserves
// distributed trace context by extracting trace information from event metadata
// and linking consumer spans to the original producer traces.
//
// Parameters:
//   - next: The underlying EventBus to be wrapped.
//   - options: Optional configuration for customizing telemetry behavior:
//   - WithAttributes: adds static attributes to all spans.
//   - WithAttributeGetter: adds dynamic attributes from context.
//   - WithOperation: customizes the span operation name.
//   - WithOperationGetter: dynamically customizes span names.
//
// Returns:
//   - A TelemetryEventBus that implements the EventBus interface with telemetry.
//
// Example Usage:
//
//	bus := otel.WithEventBusTelemetry(eventBus,
//	    otel.WithAttributes(attribute.String("service", "orders")),
//	)
//	err := bus.Subscribe(ctx, "order-projector", handler)
func WithEventBusTelemetry(next eventsourcing.EventBus, options ...Option) *TelemetryEventBus {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}

	return &TelemetryEventBus{
		next: next,
		cfg:  cfg,
	}
}
