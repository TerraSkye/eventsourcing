package logging

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/terraskye/eventsourcing"
)

// queryHandlerLogger wraps an [eventsourcing.QueryHandler] with logging.
type queryHandlerLogger[T eventsourcing.Query, R any] struct {
	logger *slog.Logger
	next   eventsourcing.QueryHandler[T, R]
}

// HandleQuery implements [eventsourcing.QueryHandler] by logging the query's
// concrete type before delegating to the wrapped handler, and logging the
// error if that call fails.
func (q *queryHandlerLogger[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	qryType := fmt.Sprintf("%T", qry)
	q.logger.InfoContext(ctx, "Query", "query", qryType)

	result, err := q.next.HandleQuery(ctx, qry)
	if err != nil {
		q.logger.ErrorContext(ctx, "Query failed", "query", qryType, "error", err)
	}

	return result, err
}

// WithQueryLogging wraps next so that every query it handles is logged: its
// concrete type before the call, and the error if the call fails.
func WithQueryLogging[T eventsourcing.Query, R any](logger *slog.Logger, next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	return &queryHandlerLogger[T, R]{
		logger: logger,
		next:   next,
	}
}

// QueryLogging returns an [eventsourcing.QueryHandlerMiddleware] that logs
// every query dispatched through a [eventsourcing.QueryBus]: its type before
// it runs, and any error it returns. Register it with
// [eventsourcing.QueryBus.Use] to apply logging to all handlers on the bus.
func QueryLogging(logger *slog.Logger) eventsourcing.QueryHandlerMiddleware {
	return func(next eventsourcing.QueryGateway[eventsourcing.Query, any]) eventsourcing.QueryGateway[eventsourcing.Query, any] {
		return WithQueryLogging(logger, next).HandleQuery
	}
}
