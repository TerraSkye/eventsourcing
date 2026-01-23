package logging

import (
	"context"
	"reflect"

	"github.com/sirupsen/logrus"
	"github.com/terraskye/eventsourcing"
)

type queryHandlerLogger[T eventsourcing.Query, R any] struct {
	logger *logrus.Entry
	next   eventsourcing.QueryHandler[T, R]
}

func (q *queryHandlerLogger[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	qryType := reflect.TypeOf(qry).String()
	q.logger.Infof("Query: %s", qryType)

	result, err := q.next.HandleQuery(ctx, qry)
	if err != nil {
		q.logger.Errorf("Query failed: %s: %v", qryType, err)
	}

	return result, err
}

// WithQueryLogging wraps a QueryHandler with logging functionality.
// It logs the query type before execution, and logs errors if the query fails.
func WithQueryLogging[T eventsourcing.Query, R any](logger *logrus.Entry, next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	return &queryHandlerLogger[T, R]{
		logger: logger,
		next:   next,
	}
}
