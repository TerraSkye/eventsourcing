package otel

import (
	"context"
	"fmt"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// QueryTelemetry returns a QueryHandlerMiddleware that instruments every query dispatched
// through the bus with OpenTelemetry tracing and metrics.
// Use with bus.Use() to apply telemetry to all registered handlers.
// The query type name is resolved at dispatch time from the concrete type.
func QueryTelemetry(options ...Option) eventsourcing.QueryHandlerMiddleware {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}

	return func(next eventsourcing.QueryGateway[eventsourcing.Query, any]) eventsourcing.QueryGateway[eventsourcing.Query, any] {
		return func(ctx context.Context, qry eventsourcing.Query) (any, error) {
			queryType := fmt.Sprintf("%T", qry)
			queryID := string(qry.ID())

			baseAttributes := []attribute.KeyValue{
				AttrQueryType.String(queryType),
				AttrQueryID.String(queryID),
			}
			baseAttributes = append(baseAttributes, cfg.Attributes...)
			if cfg.GetAttributes != nil {
				baseAttributes = append(baseAttributes, cfg.GetAttributes(ctx)...)
			}

			operation := fmt.Sprintf("query.handle %s", queryType)
			if cfg.Operation != "" {
				operation = cfg.Operation
			}
			if cfg.GetOperation != nil {
				if op := cfg.GetOperation(ctx, operation); op != "" {
					operation = op
				}
			}

			ctx, span := tracer.Start(ctx, operation,
				trace.WithSpanKind(trace.SpanKindInternal),
				trace.WithAttributes(baseAttributes...),
			)
			defer span.End()

			QueriesProcessing.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(queryType)))
			defer QueriesProcessing.Add(ctx, -1, metric.WithAttributes(AttrQueryType.String(queryType)))

			startTime := time.Now()
			result, err := next(ctx, qry)

			QueriesDuration.Record(ctx, float64(time.Since(startTime).Milliseconds()), metric.WithAttributes(AttrQueryType.String(queryType)))

			if err != nil {
				span.SetStatus(codes.Error, err.Error())
				span.RecordError(err)
				QueriesCount.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(queryType), AttrResult.String("failure")))
				return result, err
			}

			span.SetStatus(codes.Ok, "")
			QueriesCount.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(queryType), AttrResult.String("success")))
			return result, nil
		}
	}
}

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
func WithQueryTelemetry[T eventsourcing.Query, R any](next eventsourcing.QueryHandler[T, R], options ...Option) eventsourcing.QueryHandler[T, R] {
	var zero T
	queryType := fmt.Sprintf("%T", zero)

	cfg := &config{}

	for _, o := range options {
		o.apply(cfg)
	}
	return &telemetryQueryHandler[T, R]{
		next:      next,
		queryType: queryType,
		cfg:       cfg,
	}
}

type telemetryQueryHandler[T eventsourcing.Query, R any] struct {
	next      eventsourcing.QueryHandler[T, R]
	queryType string
	cfg       *config
}

func (h *telemetryQueryHandler[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {

	baseAttributes := []attribute.KeyValue{
		AttrQueryType.String(h.queryType),
		AttrQueryID.String(string(qry.ID())),
	}
	baseAttributes = append(baseAttributes, h.cfg.Attributes...)

	if h.cfg.GetAttributes != nil {
		baseAttributes = append(baseAttributes, h.cfg.GetAttributes(ctx)...)
	}

	defaultOperation := "handle query"

	if h.cfg.Operation != "" {
		defaultOperation = h.cfg.Operation
	}

	if h.cfg.GetOperation != nil {
		if op := h.cfg.GetOperation(ctx, defaultOperation); op != "" {
			defaultOperation = op
		}
	}

	ctx, span := tracer.Start(ctx, defaultOperation,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(baseAttributes...),
	)
	defer span.End()

	QueriesProcessing.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType)))
	defer QueriesProcessing.Add(ctx, -1, metric.WithAttributes(AttrQueryType.String(h.queryType)))

	startTime := time.Now()
	result, err := h.next.HandleQuery(ctx, qry)

	// Record duration metric
	QueriesDuration.Record(ctx, time.Since(startTime).Seconds(), metric.WithAttributes(AttrQueryType.String(h.queryType)))

	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
		QueriesCount.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType), AttrResult.String("failure")))
		return result, err
	}

	span.SetStatus(codes.Ok, "")
	QueriesCount.Add(ctx, 1, metric.WithAttributes(AttrQueryType.String(h.queryType), AttrResult.String("success")))

	return result, nil
}
