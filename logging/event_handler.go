package logging

import (
	"context"
	"log/slog"

	cqrs "github.com/terraskye/eventsourcing"
)

// EventLogging returns an [cqrs.EventHandlerMiddleware] that logs event
// processing. Register it with [cqrs.EventBus.Use] — for example on the bus
// returned by memory.NewEventBus or postgres.NewEventBus — to apply logging to
// every subscriber on that bus.
func EventLogging(logger *slog.Logger) cqrs.EventHandlerMiddleware {
	return func(next cqrs.EventHandler) cqrs.EventHandler {
		return WithLoggingMiddleware(logger, next)
	}
}

// WithLoggingMiddleware wraps next so that every event it handles is logged.
// It logs a debug message before and after the call, both carrying the stream
// ID, causation, version, global version, and aggregate ID found on ctx, plus
// the event's [cqrs.Event.EventType]. If next returns an error, that error is
// logged instead of the "processed successfully" message.
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
