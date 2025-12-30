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

// WithCommandTelemetry wraps a CommandHandler with OpenTelemetry tracing and metrics.
//
// This decorator observes the execution of a command handler, producing both
// tracing spans and metrics that reflect command lifecycle, success/failure,
// concurrency conflicts, and processing duration.
//
// The wrapper performs the following steps for each command execution:
//  1. Starts a span for the command handling operation, named based on the command type.
//  2. Attaches base attributes such as command type and aggregate ID.
//  3. Increments the in-flight command metric before execution and decrements it after completion.
//  4. Invokes the underlying command handler.
//  5. Updates span attributes and metrics based on the handler's result:
//     - Sets the stream ID attribute from the AppendResult.
//     - Records command duration metric.
//     - Updates span status (OK or Error).
//     - Emits metrics for handled commands, failed commands, and concurrency conflicts.
//
// Parameters:
//   - next: The underlying CommandHandler to be wrapped. It must return an AppendResult
//     containing at least the StreamID and NextExpectedVersion.
//
// Returns:
//   - A CommandHandler of the same type that automatically instruments tracing and metrics.
//
// Behavior Details:
//   - The span is started with SpanKindInternal.
//   - Base attributes include the command type and aggregate ID; the stream ID is appended after handler execution.
//   - Metrics recorded:
//   - CommandsInFlight: increments/decrements in-flight commands.
//   - CommandsDuration: duration of command handling in milliseconds.
//   - CommandsHandled: successful commands.
//   - CommandsFailed: failed commands.
//   - ConcurrencyConflicts: detected StreamRevisionConflictError occurrences.
//   - Span attributes updated after execution include stream ID and stream version.
//   - Span status is set to codes.Ok if the command succeeded, or codes.Error if an error occurred.
//
// Example Usage:
//
//	handler := WithCommandTelemetry(myCommandHandler)
//	result, err := handler(ctx, myCommand)
func WithCommandTelemetry[C eventsourcing.Command](next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {
	var zero C
	commandType := fmt.Sprintf("%T", zero)

	baseAttributes := []attribute.KeyValue{
		AttrCommandType.String(commandType),
	}

	return func(ctx context.Context, cmd C) (eventsourcing.AppendResult, error) {
		attr := append(baseAttributes, AttrAggregateID.String(cmd.AggregateID()))

		ctx, span := tracer.Start(ctx, fmt.Sprintf("command.handle %s", commandType),
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attr...),
		)
		defer span.End()

		CommandsInFlight.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
		defer CommandsInFlight.Add(ctx, -1, metric.WithAttributes(AttrCommandType.String(commandType)))
		startTime := time.Now()
		result, err := next(ctx, cmd)

		// Add streamID to attributes after execution
		attr = append(attr,
			AttrStreamID.String(result.StreamID),
			AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
		)

		// Record duration metric
		CommandsDuration.Record(ctx, float64(time.Since(startTime).Milliseconds()), metric.WithAttributes(AttrCommandType.String(commandType)))

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

			if errors.Is(err, eventsourcing.ErrBusinessRuleViolation) {
				span.SetStatus(codes.Ok, fmt.Sprintf("business rule violation: %v", err))
				span.AddEvent("business_rule_violation", trace.WithAttributes(
					AttrCommandType.String(commandType),
					AttrAggregateID.String(cmd.AggregateID()),
					AttrStreamID.String(result.StreamID),
					AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
				))
				CommandsFailed.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
				return result, err
			}

			// Real system error
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
			CommandsFailed.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))
			return result, err

		} else {
			span.SetStatus(codes.Ok, "")
		}

		CommandsHandled.Add(ctx, 1, metric.WithAttributes(AttrCommandType.String(commandType)))

		return result, err
	}
}
