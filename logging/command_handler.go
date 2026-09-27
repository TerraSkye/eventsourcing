package logging

import (
	"context"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps next so that every invocation is logged. Before
// calling next it logs the command's [eventsourcing.Command.CommandType] and
// [eventsourcing.Command.AggregateID] at info level; if next returns an error,
// that error is logged at error level as well. Attributes are only built when
// the logger is enabled for the level. A nil logger uses [slog.Default].
//
// This is where command errors get logged; callers that also log the returned
// error will log it twice.
func WithCommandLogging[C eventsourcing.Command](logger *slog.Logger, next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {
	logger = loggerOrDefault(logger)
	return func(ctx context.Context, cmd C) (eventsourcing.AppendResult, error) {
		if logger.Enabled(ctx, slog.LevelInfo) {
			logger.LogAttrs(ctx, slog.LevelInfo, "dispatching command",
				slog.String("command_type", cmd.CommandType()),
				slog.String("aggregate_id", cmd.AggregateID()),
			)
		}

		result, err := next(ctx, cmd)
		if err != nil && logger.Enabled(ctx, slog.LevelError) {
			logger.LogAttrs(ctx, slog.LevelError, "command failed",
				slog.String("command_type", cmd.CommandType()),
				slog.String("aggregate_id", cmd.AggregateID()),
				slog.Any("error", err),
			)
		}

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
