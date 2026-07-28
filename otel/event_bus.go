package otel

import (
	"context"
	"errors"
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

// TelemetryEventBus wraps an [eventsourcing.EventBus], instrumenting every
// subscription registered through it with OpenTelemetry tracing and metrics.
// For each received event it starts a consumer span linked back to the
// original producer trace, enabling end-to-end distributed tracing from
// command execution through event processing. Construct one with
// [WithEventBusTelemetry].
type TelemetryEventBus struct {
	next eventsourcing.EventBus
	cfg  *config
}

// Subscribe registers next under name on the underlying bus, wrapping it so
// each received event is handled inside a consumer span linked to the
// original producer trace (recovered from the event's metadata). The span is
// tagged with the event type, event ID, global and stream position, stream
// ID, and subscriber name; [EventBusHandled] and [EventBusDuration] are
// recorded for every invocation, and [EventBusErrors] for every error other
// than [eventsourcing.ErrSkippedEvent], which is treated as an intentional,
// non-error skip. It returns an error if the underlying bus fails to
// register the subscription.
//
//	bus := otel.WithEventBusTelemetry(eventBus)
//	err := bus.Subscribe(ctx, "order-projector", orderProjector)
func (t *TelemetryEventBus) Subscribe(ctx context.Context, name string, next eventsourcing.EventHandler, options ...eventsourcing.SubscriberOption) error {

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
			AttrEventGlobalPos.Int64(int64(eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.Int64(int64(eventsourcing.VersionFromContext(ctx))),
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

		ctx, span := tracer.Start(ctx, "receive subscription",
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithLinks(trace.Link{
				SpanContext: originalSpanContext,
				Attributes: []attribute.KeyValue{
					attribute.String("eventsourcing.link.reason", "event.consumed.from.stream"),
				},
			}),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		EventBusHandled.Add(ctx, 1, metric.WithAttributes(AttrEventType.String(event.EventType())))

		startTime := time.Now()
		err := next.Handle(ctx, event)
		EventBusDuration.Record(ctx,
			time.Since(startTime).Seconds(),
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

// Use delegates to the underlying [eventsourcing.EventBus], adding
// middlewares to the chain applied to its subscribers.
func (t *TelemetryEventBus) Use(middlewares ...eventsourcing.EventHandlerMiddleware) {
	t.next.Use(middlewares...)
}

// Errors returns the error channel from the underlying event bus. This
// method delegates directly to the wrapped [eventsourcing.EventBus] without
// additional instrumentation, since errors are already tracked at the
// handler level in [TelemetryEventBus.Subscribe].
func (t *TelemetryEventBus) Errors() <-chan error {
	return t.next.Errors()
}

// Close closes the underlying event bus and waits for all handlers to
// finish. This method delegates directly to the wrapped
// [eventsourcing.EventBus] without additional instrumentation.
func (t *TelemetryEventBus) Close() error {
	return t.next.Close()
}

// WithEventBusTelemetry wraps next in a [TelemetryEventBus], instrumenting
// every subscription registered through it with OpenTelemetry tracing and
// metrics. See [TelemetryEventBus.Subscribe] for what is recorded; options
// such as [WithAttributes] and [WithOperation] customize the spans produced.
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

// EventBusTelemetry returns an [eventsourcing.EventHandlerMiddleware] that
// instruments event handlers with OpenTelemetry tracing and metrics; it
// wraps each handler with [WithEventTelemetry]. Pass it to an
// [eventsourcing.EventBus] implementation's Use method to apply telemetry to
// all subscribers registered afterward.
func EventBusTelemetry(options ...Option) eventsourcing.EventHandlerMiddleware {
	return func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
		return WithEventTelemetry(next, options...)
	}
}
