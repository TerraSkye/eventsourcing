package logging

import (
	"context"
	"errors"
	"log/slog"
	"time"

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

// WithLoggingMiddleware wraps next so that every event it handles is logged,
// recording the same outcomes the otel package's [github.com/terraskye/eventsourcing/otel.WithEventTelemetry]
// reports on a span.
//
// It logs a debug message before the call and another once it returns, both
// carrying the stream ID, causation, version, global version, aggregate ID and
// event ID found on ctx, plus the event's [cqrs.Event.EventType]. The message
// logged afterwards also carries how long handling took:
//
//   - success: debug, "event processed successfully".
//   - [cqrs.ErrSkippedEvent]: debug rather than error, since a handler
//     declining an event type it does not want is an intentional skip — the
//     same judgement the otel package makes when it marks that span Ok.
//   - any other error: error.
func WithLoggingMiddleware(logger *slog.Logger, next cqrs.EventHandler) cqrs.EventHandler {
	return cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		l := logger.With(
			"stream-id", cqrs.StreamIDFromContext(ctx),
			"causation", cqrs.CausationFromContext(ctx),
			"version", cqrs.VersionFromContext(ctx),
			"global-version", cqrs.GlobalVersionFromContext(ctx),
			"aggregateId", cqrs.AggregateIDFromContext(ctx),
			"event", event.EventType(),
			"event-id", cqrs.EventIDFromContext(ctx),
		)

		l.DebugContext(ctx, "event processing started")

		start := time.Now()
		err := next.Handle(ctx, event)
		duration := time.Since(start)

		var skipped *cqrs.ErrSkippedEvent
		switch {
		case err == nil:
			l.DebugContext(ctx, "event processed successfully", "duration", duration)
		case errors.As(err, &skipped):
			l.DebugContext(ctx, "event skipped", "duration", duration)
		default:
			l.ErrorContext(ctx, "error processing event", "error", err, "duration", duration)
		}

		return err
	})
}
