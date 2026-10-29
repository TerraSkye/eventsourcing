package telemetry

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// CommandTelemetry returns an [eventsourcing.CommandHandlerMiddleware] that
// instruments every command dispatched through the bus with OpenTelemetry
// tracing and metrics, as [WithCommandTelemetry] describes. Use it with
// [eventsourcing.CommandBus.Use] to apply telemetry to all registered
// handlers; the command type is resolved at dispatch time from the command.
func CommandTelemetry(options ...Option) eventsourcing.CommandHandlerMiddleware {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}

	return func(next eventsourcing.CommandHandler[eventsourcing.Command]) eventsourcing.CommandHandler[eventsourcing.Command] {
		return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
			attr := []attribute.KeyValue{
				AttrCommandType.String(cmd.CommandType()),
				AttrAggregateID.String(cmd.AggregateID()),
				AttrCorrelationID.String(eventsourcing.CorrelationIDFromContext(ctx)),
				AttrCausationID.String(eventsourcing.CausationIDFromContext(ctx)),
			}
			attr = append(attr, cfg.Attributes...)

			if cfg.GetAttributes != nil {
				attr = append(attr, cfg.GetAttributes(ctx)...)
			}

			return handleCommand(ctx, cfg, cmd.CommandType(), cmd.AggregateID(), attr, func(ctx context.Context) (eventsourcing.AppendResult, error) {
				return next(ctx, cmd)
			})
		}
	}
}

// WithCommandTelemetry wraps next with OpenTelemetry tracing and metrics,
// returning an [eventsourcing.CommandHandler] of the same type that can be
// used as a drop-in replacement.
//
// Each call to the returned handler starts a span named "handle" followed by
// the command type, covering the full invocation of next, tagged with the
// command type and aggregate ID and, once next returns, with the resulting
// stream ID and version. It also records [CommandActive] (in-flight
// commands) and [CommandDuration] labelled by [AttrResult]. A returned
// [eventsourcing.StreamRevisionConflictError] is additionally counted in
// [CommandConflicts] and fails the span; a returned
// [eventsourcing.BusinessRuleViolationError] is an expected outcome rather
// than a span error, recorded with result "rejected".
//
//	handler := WithCommandTelemetry(myCommandHandler)
//	result, err := handler(ctx, myCommand)
func WithCommandTelemetry[C eventsourcing.Command](next eventsourcing.CommandHandler[C], options ...Option) eventsourcing.CommandHandler[C] {
	cfg := &config{}

	for _, o := range options {
		o.apply(cfg)
	}

	baseAttributes := []attribute.KeyValue{}
	baseAttributes = append(baseAttributes, cfg.Attributes...)

	return func(ctx context.Context, cmd C) (eventsourcing.AppendResult, error) {
		// Clone before appending: baseAttributes is shared across every call
		// to this handler, and appending directly to it would alias its
		// backing array across concurrent calls whenever it has spare
		// capacity, racing on it (see GitHub issue #59).
		attr := append(slices.Clone(baseAttributes),
			AttrAggregateID.String(cmd.AggregateID()),
			AttrCommandType.String(cmd.CommandType()),
			AttrCorrelationID.String(eventsourcing.CorrelationIDFromContext(ctx)),
			AttrCausationID.String(eventsourcing.CausationIDFromContext(ctx)),
		)

		if cfg.GetAttributes != nil {
			attr = append(attr, cfg.GetAttributes(ctx)...)
		}

		return handleCommand(ctx, cfg, cmd.CommandType(), cmd.AggregateID(), attr, func(ctx context.Context) (eventsourcing.AppendResult, error) {
			return next(ctx, cmd)
		})
	}
}

// handleCommand runs next under a span with attr and records its outcome,
// for both [CommandTelemetry] and [WithCommandTelemetry].
func handleCommand(ctx context.Context, cfg *config, commandType, aggregateID string, attr []attribute.KeyValue, next func(context.Context) (eventsourcing.AppendResult, error)) (eventsourcing.AppendResult, error) {
	operation := "handle " + commandType
	if cfg.Operation != "" {
		operation = cfg.Operation
	}
	if cfg.GetOperation != nil {
		if op := cfg.GetOperation(ctx, operation); op != "" {
			operation = op
		}
	}

	ctx, span := tracer.Start(ctx, operation,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attr...),
	)
	defer span.End()

	typeAttr := AttrCommandType.String(commandType)
	CommandActive.Add(ctx, 1, metric.WithAttributes(typeAttr))
	defer CommandActive.Add(ctx, -1, metric.WithAttributes(typeAttr))
	startTime := time.Now()
	result, err := next(ctx)
	duration := time.Since(startTime).Seconds()

	span.SetAttributes(
		AttrStreamID.String(result.StreamID),
		AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
	)

	outcome := "success"
	var conflict *eventsourcing.StreamRevisionConflictError
	var violation *eventsourcing.BusinessRuleViolationError
	switch {
	case err == nil:
		span.SetStatus(codes.Ok, "")
	case errors.As(err, &violation):
		// A rejection is an expected outcome, not a failure of the span.
		outcome = "rejected"
		causeMessage := "business rule violation"
		if cause := violation.Cause(); cause != nil {
			causeMessage = cause.Error()
		}
		span.SetAttributes(AttrErrorMessage.String(causeMessage))
		span.SetStatus(codes.Ok, "")
		span.AddEvent("business_rule_violation", trace.WithAttributes(
			typeAttr,
			AttrAggregateID.String(aggregateID),
			AttrStreamID.String(result.StreamID),
			AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
			AttrErrorMessage.String(causeMessage),
		))
	default:
		outcome = "failure"
		if errors.As(err, &conflict) {
			// A conflict left after any retries is a failure too.
			CommandConflicts.Add(ctx, 1, metric.WithAttributes(typeAttr))
			span.AddEvent("concurrency_conflict", trace.WithAttributes(AttrStreamID.String(result.StreamID)))
		}
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	}

	CommandDuration.Record(ctx, duration, metric.WithAttributes(typeAttr, AttrResult.String(outcome)))
	return result, err
}
