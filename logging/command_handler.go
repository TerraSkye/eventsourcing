package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps a CommandHandler with logging functionality.
// It logs the command type and aggregate ID before execution, and logs
// errors if the command fails.
func WithCommandLogging[C eventsourcing.Command](logger *slog.Logger, next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {

	return func(ctx context.Context, command C) (eventsourcing.AppendResult, error) {
		cmdType := fmt.Sprintf("%T", command)
		logger.InfoContext(ctx, "Dispatch", "command", cmdType, "aggregateID", command.AggregateID())

		result, err := next(ctx, command)
		if err != nil {
			logger.ErrorContext(ctx, "Dispatch failed", "command", cmdType, "aggregateID", command.AggregateID(), "error", err)
		}

		return result, err
	}
}

// CommandLogging returns a CommandBusMiddleware that logs every command dispatched
// through the bus. Use with bus.Use() to apply logging to all registered handlers.
func CommandLogging(logger *slog.Logger) eventsourcing.CommandBusMiddleware {
	return func(next func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error)) func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
		return func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
			cmdType := fmt.Sprintf("%T", cmd)
			logger.InfoContext(ctx, "Dispatch", "command", cmdType, "aggregateID", cmd.AggregateID())
			result, err := next(ctx, cmd)
			if err != nil {
				logger.ErrorContext(ctx, "Dispatch failed", "command", cmdType, "aggregateID", cmd.AggregateID(), "error", err)
			}
			return result, err
		}
	}
}
