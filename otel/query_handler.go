package otel

import (
	"context"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// WithQueryTelemetry wraps a QueryHandler with OpenTelemetry tracing and metrics.
//
// This decorator observes the execution of a query handler, producing both
// tracing spans and metrics that reflect query lifecycle, success/failure,
// and processing duration.
//
// The wrapper performs the following steps for each query execution:
//  1. Starts a span for the query handling operation, named based on the query type.
//  2. Attaches base attributes such as query type and query ID.
//  3. Increments the in-flight query metric before execution and decrements it after completion.
//  4. Invokes the underlying query handler.
//  5. Updates span attributes and metrics based on the handler's result:
//     - Records query duration metric.
//     - Updates span status (OK or Error).
//     - Emits metrics for handled queries and failed queries.
//
// Example Usage:
//
//	handler := WithQueryTelemetry(myQueryHandler)
//	result, err := handler.HandleQuery(ctx, myQuery)
func WithQueryTelemetry[T eventsourcing.Query, R any](next eventsourcing.QueryHandler[T, R]) eventsourcing.QueryHandler[T, R] {
	var zero T
	queryType := fmt.Sprintf("%T", zero)

	return &telemetryQueryHandler[T, R]{
		next:      next,
		queryType: queryType,
		tracer:    otel.Tracer("eventsourcing"),
	}
}

type telemetryQueryHandler[T eventsourcing.Query, R any] struct {
	next      eventsourcing.QueryHandler[T, R]
	queryType string
	tracer    trace.Tracer
}

func (h *telemetryQueryHandler[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	ctx, span := h.tracer.Start(ctx, fmt.Sprintf("query.handle %s", h.queryType),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrQueryType.String(h.queryType),
			AttrQueryID.String(string(qry.ID())),
		),
	)
	defer span.End()

	QueriesInFlight.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType)))
	defer QueriesInFlight.Add(ctx, -1, metric.WithAttributes(AttrQueryType.String(h.queryType)))

	startTime := time.Now()
	result, err := h.next.HandleQuery(ctx, qry)

	// Record duration metric
	QueriesDuration.Record(ctx, float64(time.Since(startTime).Milliseconds()), metric.WithAttributes(AttrQueryType.String(h.queryType)))

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		QueriesFailed.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType)))
		return result, err
	}

	span.SetStatus(codes.Ok, "")
	QueriesHandled.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType)))

	return result, nil
}
