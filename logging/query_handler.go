package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// WithQueryLogging wraps next so that every query it handles is logged: its
// concrete type at info level before the call, and the error at error level
// if the call fails. Attributes are only built when the logger is enabled for
// the level. A nil logger uses [slog.Default].
//
// This is where query errors get logged; callers that also log the returned
// error will log it twice.
func WithQueryLogging[T eventsourcing.Query, R any](logger *slog.Logger, next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	logger = loggerOrDefault(logger)
	return eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, qry T) (R, error) {
		if logger.Enabled(ctx, slog.LevelInfo) {
			logger.LogAttrs(ctx, slog.LevelInfo, "handling query",
				slog.String("query_type", fmt.Sprintf("%T", qry)),
			)
		}

		result, err := next.HandleQuery(ctx, qry)
		if err != nil && logger.Enabled(ctx, slog.LevelError) {
			logger.LogAttrs(ctx, slog.LevelError, "query failed",
				slog.String("query_type", fmt.Sprintf("%T", qry)),
				slog.Any("error", err),
			)
		}

		return result, err
	})
}

// QueryLogging returns an [eventsourcing.QueryHandlerMiddleware] that logs
// every query dispatched through a [eventsourcing.QueryBus], as described on
// [WithQueryLogging]. Register it with [eventsourcing.QueryBus.Use] to apply
// logging to all handlers on the bus.
func QueryLogging(logger *slog.Logger) eventsourcing.QueryHandlerMiddleware {
	return func(next eventsourcing.QueryGateway[eventsourcing.Query, any]) eventsourcing.QueryGateway[eventsourcing.Query, any] {
		return WithQueryLogging(logger, next).HandleQuery
	}
}
