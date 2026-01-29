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

// TODO extract the consumer group
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
			AttrEventGlobalPos.String(fmt.Sprintf("%d", eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.String(fmt.Sprintf("%d", eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
		}

		attr = append(attr, cfg.Attributes...)

		if cfg.GetAttributes != nil {
			attr = append(attr, cfg.GetAttributes(ctx)...)
		}

		// Extract the SpanContext from the original trace
		originalCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		originalSpanContext := trace.SpanContextFromContext(originalCtx)

		operationName := fmt.Sprintf("events.handle %s", event.EventType())

		ctx, span := tracer.Start(ctx, operationName,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithLinks(trace.Link{
				SpanContext: originalSpanContext,
				Attributes: []attribute.KeyValue{
					attribute.String("link.reason", "event.consumed.from.stream"),
				},
			}),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		// Add the original trace ID as an attribute for easier correlation
		if originalSpanContext.IsValid() {
			span.SetAttributes(
				attribute.String("original.trace.id", originalSpanContext.TraceID().String()),
				attribute.String("original.span.id", originalSpanContext.SpanID().String()),
			)
		}

		EventBusHandled.Add(ctx, 1, metric.WithAttributes(AttrEventType.String(event.EventType())))

		startTime := time.Now()
		err := next.Handle(ctx, event)
		EventBusDuration.Record(ctx,
			float64(time.Since(startTime).Milliseconds()),
			metric.WithAttributes(AttrEventType.String(event.EventType())),
		)

		if err != nil {
			if errors.Is(err, &eventsourcing.ErrSkippedEvent{}) {
				span.SetStatus(codes.Ok, "")
			} else {
				span.SetStatus(codes.Error, err.Error())
				span.RecordError(err)
			}
			return err
		}
		span.SetStatus(codes.Ok, "")
		return nil
	})
}
