package logging

import (
	"context"
	"log/slog"

	cqrs "github.com/terraskye/eventsourcing"
)

// EventLogging returns an EventHandlerMiddleware that logs event processing.
// Use with NewMiddlewareEventBus().Use() to apply logging to all subscribers.
func EventLogging(logger *slog.Logger) cqrs.EventHandlerMiddleware {
	return func(next cqrs.EventHandler) cqrs.EventHandler {
		return WithLoggingMiddleware(logger, next)
	}
}

func WithLoggingMiddleware(logger *slog.Logger, next cqrs.EventHandler) cqrs.EventHandler {
	return cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		l := logger.With(
			"stream-id", cqrs.StreamIDFromContext(ctx),
			"causation", cqrs.CausationFromContext(ctx),
			"version", cqrs.VersionFromContext(ctx),
			"global-version", cqrs.GlobalVersionFromContext(ctx),
			"aggregateId", cqrs.AggregateIDFromContext(ctx),
			"event", event.EventType(),
		)

		l.DebugContext(ctx, "event processing started")

		err := next.Handle(ctx, event)

		if err != nil {
			l.ErrorContext(ctx, "error processing event", "error", err)
		} else {
			l.DebugContext(ctx, "event processed successfully")
		}

		return err

	})
}
