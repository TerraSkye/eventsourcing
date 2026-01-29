package logging

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/terraskye/eventsourcing"
)

type queryHandlerLogger[T eventsourcing.Query, R any] struct {
	logger *slog.Logger
	next   eventsourcing.QueryHandler[T, R]
}

func (q *queryHandlerLogger[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	qryType := reflect.TypeOf(qry).String()
	q.logger.InfoContext(ctx, "Query", "query", qryType)

	result, err := q.next.HandleQuery(ctx, qry)
	if err != nil {
		q.logger.ErrorContext(ctx, "Query failed", "query", qryType, "error", err)
	}

	return result, err
}

// WithQueryLogging wraps a QueryHandler with logging functionality.
// It logs the query type before execution, and logs errors if the query fails.
func WithQueryLogging[T eventsourcing.Query, R any](logger *slog.Logger, next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	return &queryHandlerLogger[T, R]{
		logger: logger,
		next:   next,
	}
}
