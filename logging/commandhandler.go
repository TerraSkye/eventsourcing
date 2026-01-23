package logging

import (
	"context"
	"reflect"

	"github.com/sirupsen/logrus"
	"github.com/terraskye/eventsourcing"
)

// WithCommandLogging wraps a CommandHandler with logging functionality.
// It logs the command type and aggregate ID before execution, and logs
// errors if the command fails.
func WithCommandLogging[C eventsourcing.Command](logger *logrus.Entry, next eventsourcing.CommandHandler[C]) eventsourcing.CommandHandler[C] {
	return func(ctx context.Context, command C) (eventsourcing.AppendResult, error) {
		cmdType := reflect.TypeOf(command).String()
		logger.Infof("Dispatch: %s (aggregateID: %s)", cmdType, command.AggregateID())

		result, err := next(ctx, command)
		if err != nil {
			logger.Errorf("Dispatch failed: %s (aggregateID: %s): %v", cmdType, command.AggregateID(), err)
		}

		return result, err
	}
}
