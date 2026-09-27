package logging

import (
	"context"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// EventLogging returns an [eventsourcing.EventHandlerMiddleware] that logs
// event processing, as described on [WithEventLogging]. Register it with
// [eventsourcing.EventBus.Use] — for example on the bus returned by
// memory.NewEventBus or postgres.NewEventBus — to apply logging to every
// subscriber on that bus.
func EventLogging(logger *slog.Logger) eventsourcing.EventHandlerMiddleware {
	return func(next eventsourcing.EventHandler) eventsourcing.EventHandler {
		return WithEventLogging(logger, next)
	}
}

// WithEventLogging wraps next so that every event it handles is logged. It
// logs a debug message before and after the call, both carrying the stream
// ID, causation, version, global version, and aggregate ID found on ctx, plus
// the event's [eventsourcing.Event.EventType]. If next returns an error, that
// error is logged at error level instead of the second debug message.
// Attributes are only built when the logger is enabled for the level. A nil
// logger uses [slog.Default].
//
// This is where event handler errors get logged; callers that also log the
// returned error will log it twice.
func WithEventLogging(logger *slog.Logger, next eventsourcing.EventHandler) eventsourcing.EventHandler {
	logger = loggerOrDefault(logger)
	return eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		debug := logger.Enabled(ctx, slog.LevelDebug)
		if debug {
			logger.LogAttrs(ctx, slog.LevelDebug, "handling event", eventAttrs(ctx, event)...)
		}

		err := next.Handle(ctx, event)

		switch {
		case err != nil:
			if logger.Enabled(ctx, slog.LevelError) {
				logger.LogAttrs(ctx, slog.LevelError, "event failed", append(eventAttrs(ctx, event), slog.Any("error", err))...)
			}
		case debug:
			logger.LogAttrs(ctx, slog.LevelDebug, "event handled", eventAttrs(ctx, event)...)
		}

		return err
	})
}

// WithLoggingMiddleware wraps next so that every event it handles is logged.
//
// Deprecated: Use [WithEventLogging].
func WithLoggingMiddleware(logger *slog.Logger, next eventsourcing.EventHandler) eventsourcing.EventHandler {
	return WithEventLogging(logger, next)
}

// eventAttrs returns the attributes logged for event, read from ctx.
func eventAttrs(ctx context.Context, event eventsourcing.Event) []slog.Attr {
	return []slog.Attr{
		slog.String("event_type", event.EventType()),
		slog.String("stream_id", eventsourcing.StreamIDFromContext(ctx)),
		slog.String("aggregate_id", eventsourcing.AggregateIDFromContext(ctx)),
		slog.Uint64("version", eventsourcing.VersionFromContext(ctx)),
		slog.Uint64("global_version", eventsourcing.GlobalVersionFromContext(ctx)),
		slog.String("causation", eventsourcing.CausationFromContext(ctx)),
	}
}
