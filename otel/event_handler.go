package otel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// TODO extract the consumer group
func WithEventTelemetry(next eventsourcing.EventHandler) eventsourcing.EventHandler {

	return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {

		attr := []attribute.KeyValue{
			AttrEventType.String(event.EventType()),
			AttrEventID.String(eventsourcing.EventIDFromContext(ctx).String()),
			AttrEventGlobalPos.String(fmt.Sprintf("%d", eventsourcing.GlobalVersionFromContext(ctx))),
			AttrEventStreamPos.String(fmt.Sprintf("%d", eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
		}

		ctx, span := tracer.Start(ctx, fmt.Sprintf("events.handle %s", event.EventType()),
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		EventBusHandled.Add(ctx, 1, metric.WithAttributes(AttrEventType.String(event.EventType())))
		defer EventBusHandled.Add(ctx, -1, metric.WithAttributes(AttrEventType.String(event.EventType())))

		startTime := time.Now()
		err := next.Handle(ctx, event)
		EventBusDuration.Record(ctx,
			float64(time.Since(startTime).Milliseconds()),
			metric.WithAttributes(AttrEventType.String(event.EventType())),
		)

		if err != nil {
			if errors.Is(err, &eventsourcing.ErrSkippedEvent{}) {
				span.SetStatus(codes.Ok, "event skipped")
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
