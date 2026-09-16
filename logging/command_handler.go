package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps next so that every invocation is logged, recording
// the same outcomes the otel package's [github.com/terraskye/eventsourcing/otel.WithCommandTelemetry]
// reports on a span.
//
// Before calling next it logs the command's concrete type and
// [eventsourcing.Command.AggregateID] at info level. Once next returns it logs
// a second time, always carrying how long the call took, so a slow command can
// be found from the logs alone:
//
//   - success: info, with the [eventsourcing.AppendResult]'s stream ID and
//     next expected version.
//   - [eventsourcing.ErrBusinessRuleViolation]: warn rather than error, since
//     a violated business rule is an expected domain outcome — the same
//     judgement the otel package makes when it marks that span Ok rather than
//     failed.
//   - [eventsourcing.StreamRevisionConflictError]: error, with the expected
//     and actual revisions, which is what a command whose conflict retries ran
//     out ends with.
//   - any other error: error.
func WithCommandLogging[C eventsourcing.Command](l *slog.Logger, next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {
	if l == nil {
		l = slog.Default()
	}
	return func(ctx context.Context, command C) (eventsourcing.AppendResult, error) {

		if l.Enabled(ctx, slog.LevelInfo) {
			l.InfoContext(ctx, "Dispatch",
				"command", fmt.Sprintf("%T", command),
				"aggregateID", command.AggregateID(),
			)
		}
		start := time.Now()
		result, err := next(ctx, command)
		duration := slog.DurationValue(time.Since(start))
		if err == nil {
			if l.Enabled(ctx, slog.LevelInfo) {
				l.InfoContext(ctx, "Dispatch succeeded",
					"streamID", result.StreamID,
					"version", result.NextExpectedVersion,
					"duration", duration,
					"command", fmt.Sprintf("%T", command),
					"aggregateID", command.AggregateID(),
				)
			}
			return result, nil
		}
		var violation *eventsourcing.ErrBusinessRuleViolation
		if errors.As(err, &violation) {
			reason := "business rule violation"
			if cause := violation.Cause(); cause != nil {
				reason = cause.Error()
			}
			l.WarnContext(ctx, "Dispatch rejected",
				"error", err,
				"reason", reason,
				"streamID", result.StreamID,
				"duration", duration,
				"command", fmt.Sprintf("%T", command),
				"aggregateID", command.AggregateID(),
			)
			return result, err
		}

		var conflict *eventsourcing.StreamRevisionConflictError
		if errors.As(err, &conflict) {
			// Either revision may be nil on a hand-built error, so they are
			// resolved to a raw value only when set rather than rendered
			// through a nil interface.
			var expected, actual any
			if conflict.ExpectedRevision != nil {
				expected = conflict.ExpectedRevision.ToRawInt64()
			}
			if conflict.ActualRevision != nil {
				actual = conflict.ActualRevision.ToRawInt64()
			}

			l.ErrorContext(ctx, "Dispatch failed",
				"error", err,
				"conflict", "stream revision",
				"streamID", result.StreamID,
				"expectedRevision", expected,
				"actualRevision", actual,
				"duration", duration,
				"command", fmt.Sprintf("%T", command),
				"aggregateID", command.AggregateID(),
			)
			return result, err
		}

		l.ErrorContext(ctx, "Dispatch failed",
			"error", err,
			"streamID", result.StreamID,
			"duration", duration,
			"command", fmt.Sprintf("%T", command),
			"aggregateID", command.AggregateID(),
		)

		return result, err
	}
}

// CommandLogging returns an [eventsourcing.CommandHandlerMiddleware] that logs
// every command dispatched through a [eventsourcing.CommandBus], as described
// on [WithCommandLogging]. Register it with [eventsourcing.CommandBus.Use] to
// apply logging to all handlers on the bus.
func CommandLogging(logger *slog.Logger) eventsourcing.CommandHandlerMiddleware {
	return func(next eventsourcing.CommandHandler[eventsourcing.Command]) eventsourcing.CommandHandler[eventsourcing.Command] {
		return WithCommandLogging(logger, next)
	}
}
