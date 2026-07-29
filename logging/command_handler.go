package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps next so that every invocation is logged. Before
// calling next it logs the command's concrete type and
// [eventsourcing.Command.AggregateID]; if next returns an error, that error is
// logged as well.
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

// CommandLogging returns an [eventsourcing.CommandHandlerMiddleware] that logs
// every command dispatched through a [eventsourcing.CommandBus]: its type and
// aggregate ID before it runs, and any error it returns. Register it with
// [eventsourcing.CommandBus.Use] to apply logging to all handlers on the bus.
func CommandLogging(logger *slog.Logger) eventsourcing.CommandHandlerMiddleware {
	return func(next eventsourcing.CommandHandler[eventsourcing.Command]) eventsourcing.CommandHandler[eventsourcing.Command] {
		return WithCommandLogging(logger, next)
	}
}
