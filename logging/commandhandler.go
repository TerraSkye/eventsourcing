package logging

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps a CommandHandler with logging functionality.
// It logs the command type and aggregate ID before execution, and logs
// errors if the command fails.
func WithCommandLogging[C eventsourcing.Command](logger *slog.Logger, next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {
	return func(ctx context.Context, command C) (eventsourcing.AppendResult, error) {
		cmdType := reflect.TypeOf(command).String()
		logger.InfoContext(ctx, "Dispatch", "command", cmdType, "aggregateID", command.AggregateID())

		result, err := next(ctx, command)
		if err != nil {
			logger.ErrorContext(ctx, "Dispatch failed", "command", cmdType, "aggregateID", command.AggregateID(), "error", err)
		}

		return result, err
	}
}
