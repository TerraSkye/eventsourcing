package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/projection"
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
// from the event's metadata, when it carries one), tagged with the event type, event ID, global
// and stream position, and stream ID. It records [EventHandlerDuration] for
// every invocation, labelled by [AttrResult]. A returned
// [eventsourcing.SkippedEventError] is treated as an intentional, non-error
// skip: it marks the span OK and is recorded with result "skipped". Any
// other error marks the span as failed and is recorded with result
// "failure" and an [AttrErrorType].
//
// When next is a [projection.Definition]'s handler and the runner commits
// through a store wrapped with [WithProjectionStoreTelemetry], the span is a
// child of the batch's span and is tagged with the projection's name and
// segment, and the metrics with its name.
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
			AttrEventGlobalPos.Int64(int64(eventsourcing.GlobalPositionFromContext(ctx))),
			AttrEventStreamPos.Int64(int64(eventsourcing.VersionFromContext(ctx))),
			AttrStreamID.String(eventsourcing.StreamIDFromContext(ctx)),
			AttrCorrelationID.String(eventsourcing.CorrelationIDFromContext(ctx)),
		}

		metricAttr := []attribute.KeyValue{AttrEventType.String(event.EventType())}
		if l, ok := ctx.Value(leaseKey{}).(projection.Lease); ok {
			attr = append(attr, leaseAttrs(l)...)
			metricAttr = append(metricAttr, AttrProjectionName.String(l.Name))
		}

		attr = append(attr, cfg.Attributes...)

		if cfg.GetAttributes != nil {
			attr = append(attr, cfg.GetAttributes(ctx)...)
		}

		// Extract the SpanContext from the original trace
		originalCtx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
		originalSpanContext := trace.SpanContextFromContext(originalCtx)

		defaultOperation := "process event"
		if cfg.Operation != "" {
			defaultOperation = cfg.Operation
		}
		operation := defaultOperation
		if cfg.GetOperation != nil {
			if op := cfg.GetOperation(ctx, defaultOperation); op != "" {
				operation = op
			}
		}

		opts := []trace.SpanStartOption{
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attr...),
		}
		// An event saved without a trace has no producer to link to; the SDK
		// would still record an empty link for the link's attributes.
		if originalSpanContext.IsValid() {
			opts = append(opts, trace.WithLinks(trace.Link{
				SpanContext: originalSpanContext,
				Attributes: []attribute.KeyValue{
					AttrLinkReason.String("event.consumed.from.stream"),
				},
			}))
		}
		ctx, span := tracer.Start(ctx, operation, opts...)
		defer span.End()

		startTime := time.Now()
		err := next.Handle(ctx, event)
		duration := time.Since(startTime).Seconds()

		var skipped *eventsourcing.SkippedEventError
		switch {
		case err == nil:
			metricAttr = append(metricAttr, AttrResult.String("success"))
			span.SetStatus(codes.Ok, "")
		case errors.As(err, &skipped):
			metricAttr = append(metricAttr, AttrResult.String("skipped"))
			span.SetStatus(codes.Ok, "")
		default:
			errType := AttrErrorType.String(errorType(err))
			metricAttr = append(metricAttr, AttrResult.String("failure"), errType)
			span.SetAttributes(errType)
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		}
		EventHandlerDuration.Record(ctx, duration, metric.WithAttributes(metricAttr...))
		return err
	})
}

// EventTelemetry returns an [eventsourcing.EventHandlerMiddleware] that
// wraps each handler with [WithEventTelemetry].
func EventTelemetry(options ...Option) eventsourcing.EventHandlerMiddleware {
	return func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
		return WithEventTelemetry(next, options...)
	}
}
