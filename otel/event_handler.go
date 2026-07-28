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

// WithEventTelemetry wraps next with OpenTelemetry tracing and metrics,
// returning an [eventsourcing.EventHandler] that can be used as a drop-in
// replacement.
//
// Each call starts a span linked to the original producer trace (recovered
// from the event's metadata), tagged with the event type, event ID, global
// and stream position, and stream ID. It records [EventBusHandled] and
// [EventBusDuration] for every invocation. A returned
// [eventsourcing.ErrSkippedEvent] is treated as an intentional, non-error
// skip and marks the span OK, while any other error marks the span as
// failed; unlike [TelemetryEventBus.Subscribe], it does not record
// [EventBusErrors].
//
// TODO: extract the consumer group.
func WithEventTelemetry(next eventsourcing.EventHandler, options ...Option) eventsourcing.EventHandler {
	cfg := &config{}

	for _, o := range options {
		o.apply(cfg)
	}
	return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {

		// Extract the original trace context from event metadata
		var carrier = make(propagation.MapCarrier)
		if metadata := eventsourcing.MetadataFromContext(ctx); len(metadata) > 0 {
			for k, v := range metadata {
				if stringV, ok := v.(string); ok && len(stringV) > 0 {
					carrier[k] = stringV
				}
			}
		} else {
			carrier = make(propagation.MapCarrier)
		}

		attr := []attribute.KeyValue{
			AttrEventType.String(event.EventType()),
			AttrEventID.String(eventsourcing.EventIDFromContext(ctx).String()),
			AttrEventGlobalPos.Int64(int64(eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.Int64(int64(eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
		}

		attr = append(attr, cfg.Attributes...)

		if cfg.GetAttributes != nil {
			attr = append(attr, cfg.GetAttributes(ctx)...)
		}

		// Extract the SpanContext from the original trace
		originalCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		originalSpanContext := trace.SpanContextFromContext(originalCtx)

		ctx, span := tracer.Start(ctx, "process event",
			trace.WithSpanKind(trace.SpanKindInternal),
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
				// TODO: TelemetryEventBus.Subscribe increments EventBusErrors
				// here; this path does not. Confirm whether that's
				// intentional or a gap.
				span.SetStatus(codes.Error, err.Error())
				span.RecordError(err)
			}
			return err
		}
		span.SetStatus(codes.Ok, "")
		return nil
	})
}
