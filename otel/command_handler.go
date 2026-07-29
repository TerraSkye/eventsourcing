package otel

import (
	"context"
	"errors"
	"fmt"
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
// tracing and metrics. Use it with [eventsourcing.CommandBus.Use] to apply
// telemetry to all registered handlers; the command type name is resolved at
// dispatch time from the concrete type.
func CommandTelemetry(options ...Option) eventsourcing.CommandHandlerMiddleware {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}

	return func(next eventsourcing.CommandHandler[eventsourcing.Command]) eventsourcing.CommandHandler[eventsourcing.Command] {
		return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
			commandType := fmt.Sprintf("%T", cmd)
			ctx = eventsourcing.WithCausation(ctx, commandType)

			attr := []attribute.KeyValue{
				AttrCommandType.String(commandType),
				AttrAggregateID.String(cmd.AggregateID()),
			}
			attr = append(attr, cfg.Attributes...)

			if cfg.GetAttributes != nil {
				attr = append(attr, cfg.GetAttributes(ctx)...)
			}

			operation := fmt.Sprintf("command.handle %s", commandType)
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

			CommandsProcessing.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
			defer CommandsProcessing.Add(ctx, -1, metric.WithAttributes(AttrCommandType.String(commandType)))
			startTime := time.Now()
			result, err := next(ctx, cmd)

			attr = append(attr,
				AttrStreamID.String(result.StreamID),
				AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
			)
			CommandsDuration.Record(ctx, float64(time.Since(startTime).Milliseconds()), metric.WithAttributes(AttrCommandType.String(commandType)))
			span.SetAttributes(attr...)

			if err != nil {
				var conflict *eventsourcing.StreamRevisionConflictError
				if errors.As(err, &conflict) {
					ConcurrencyConflicts.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
					span.AddEvent("concurrency_conflict", trace.WithAttributes(AttrStreamID.String(result.StreamID)))
				}
				var businessViolation *eventsourcing.ErrBusinessRuleViolation
				if errors.As(err, &businessViolation) {
					causeMessage := "business rule violation"
					if cause := businessViolation.Cause(); cause != nil {
						causeMessage = cause.Error()
					}
					span.SetAttributes(AttrErrorMessage.String(causeMessage))
					span.SetStatus(codes.Ok, "")
					span.AddEvent("business_rule_violation", trace.WithAttributes(
						AttrCommandType.String(commandType),
						AttrAggregateID.String(cmd.AggregateID()),
						AttrStreamID.String(result.StreamID),
						AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
						AttrErrorMessage.String(causeMessage),
					))
					CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("failure")))
					return result, err
				}
				span.SetStatus(codes.Error, err.Error())
				span.RecordError(err)
				CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("failure")))
				return result, err
			}

			span.SetStatus(codes.Ok, "")
			CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("success")))
			return result, err
		}
	}
}

// WithCommandTelemetry wraps next with OpenTelemetry tracing and metrics,
// returning an [eventsourcing.CommandHandler] of the same type that can be
// used as a drop-in replacement.
//
// Each call to the returned handler starts a span covering the full
// invocation of next, tagged with the command type and aggregate ID and, once
// next returns, with the resulting stream ID and version. It also records
// [CommandsProcessing] (in-flight commands), [CommandsDuration], and
// [CommandsCount] labelled by [AttrResult]. A returned
// [eventsourcing.StreamRevisionConflictError] is additionally counted in
// [ConcurrencyConflicts]; a returned [eventsourcing.ErrBusinessRuleViolation]
// is recorded on the span as an expected outcome rather than a span error.
//
//	handler := WithCommandTelemetry(myCommandHandler)
//	result, err := handler(ctx, myCommand)
func WithCommandTelemetry[C eventsourcing.Command](next eventsourcing.CommandHandler[C], options ...Option) eventsourcing.CommandHandler[C] {
	cfg := &config{}

	for _, o := range options {
		o.apply(cfg)
	}

	var zero C
	commandType := fmt.Sprintf("%T", zero)

	baseAttributes := []attribute.KeyValue{
		AttrCommandType.String(commandType),
	}
	baseAttributes = append(baseAttributes, cfg.Attributes...)

	defaultOperation := "handle command"
	if cfg.Operation != "" {
		defaultOperation = cfg.Operation
	}

	return func(ctx context.Context, cmd C) (eventsourcing.AppendResult, error) {
		ctx = eventsourcing.WithCausation(ctx, commandType)
		// Clone before appending: baseAttributes is shared across every call
		// to this handler, and appending directly to it would alias its
		// backing array across concurrent calls whenever it has spare
		// capacity, racing on it (see GitHub issue #59).
		attr := append(slices.Clone(baseAttributes), AttrAggregateID.String(cmd.AggregateID()))

		if cfg.GetAttributes != nil {
			attr = append(attr, cfg.GetAttributes(ctx)...)
		}

		operation := defaultOperation
		if cfg.GetOperation != nil {
			if op := cfg.GetOperation(ctx, defaultOperation); op != "" {
				operation = op
			}
		}

		ctx, span := tracer.Start(ctx, operation,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		CommandsProcessing.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
		defer CommandsProcessing.Add(ctx, -1, metric.WithAttributes(AttrCommandType.String(commandType)))
		startTime := time.Now()
		result, err := next(ctx, cmd)

		// Add streamID to attributes after execution
		attr = append(attr,
			AttrStreamID.String(result.StreamID),
			AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
		)

		// Record duration metric
		CommandsDuration.Record(ctx, time.Since(startTime).Seconds(), metric.WithAttributes(AttrCommandType.String(commandType)))

		// Update span attributes
		span.SetAttributes(attr...)

		if err != nil {
			var conflict *eventsourcing.StreamRevisionConflictError
			if errors.As(err, &conflict) {
				ConcurrencyConflicts.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
				// Span is still considered successful (operation executed)
				span.AddEvent("concurrency_conflict", trace.WithAttributes(
					AttrStreamID.String(result.StreamID),
				))
			}
			var bussinessViolation *eventsourcing.ErrBusinessRuleViolation
			if errors.As(err, &bussinessViolation) {

				causeMessage := "business rule violation"
				if cause := bussinessViolation.Cause(); cause != nil {
					causeMessage = cause.Error()
				}
				span.SetAttributes(AttrErrorMessage.String(causeMessage))
				span.SetStatus(codes.Ok, "")
				span.AddEvent("business_rule_violation", trace.WithAttributes(
					AttrCommandType.String(commandType),
					AttrAggregateID.String(cmd.AggregateID()),
					AttrStreamID.String(result.StreamID),
					AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
					AttrErrorMessage.String(causeMessage),
				))
				CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("failure")))
				return result, err
			}

			// Real system error
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("failure")))
			return result, err

		} else {
			span.SetStatus(codes.Ok, "")
		}

		CommandsCount.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType), AttrResult.String("success")))

		return result, err
	}
}
