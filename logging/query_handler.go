package logging

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/terraskye/eventsourcing"
)

// queryHandlerLogger wraps an [eventsourcing.QueryHandler] with logging.
type queryHandlerLogger[T eventsourcing.Query, R any] struct {
	logger *slog.Logger
	next   eventsourcing.QueryHandler[T, R]
}

// HandleQuery implements [eventsourcing.QueryHandler] by logging the query's
// concrete type and [eventsourcing.Query.ID] before delegating to the wrapped
// handler, then logging the outcome — success at info, failure at error —
// with how long the call took.
func (q *queryHandlerLogger[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	l := q.logger.With(
		"query", fmt.Sprintf("%T", qry),
		"queryID", string(qry.ID()),
	)
	l.InfoContext(ctx, "Query")

	start := time.Now()
	result, err := q.next.HandleQuery(ctx, qry)
	duration := time.Since(start)

	if err != nil {
		l.ErrorContext(ctx, "Query failed", "error", err, "duration", duration)
		return result, err
	}

	l.InfoContext(ctx, "Query succeeded", "duration", duration)

	return result, nil
}

// WithQueryLogging wraps next so that every query it handles is logged: its
// concrete type and ID before the call, and its outcome and duration after,
// mirroring what the otel package's [github.com/terraskye/eventsourcing/otel.WithQueryTelemetry] records for the
// same handler.
func WithQueryLogging[T eventsourcing.Query, R any](logger *slog.Logger, next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	return &queryHandlerLogger[T, R]{
		logger: logger,
		next:   next,
	}
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
