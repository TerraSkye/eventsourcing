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

// QueryTelemetry returns an [eventsourcing.QueryHandlerMiddleware] that
// instruments every query dispatched through the bus with OpenTelemetry
// tracing and metrics. Use it with [eventsourcing.QueryBus.Use] to apply
// telemetry to all registered handlers; the query type name is resolved at
// dispatch time from the concrete type.
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

// WithQueryTelemetry wraps next with OpenTelemetry tracing and metrics,
// returning an [eventsourcing.QueryHandler] of the same type that can be
// used as a drop-in replacement.
//
// Each call to the returned handler's HandleQuery starts a span covering the
// full invocation of next, tagged with the query type and query ID. It also
// records [QueriesProcessing] (in-flight queries), [QueriesDuration], and
// [QueriesCount] labelled by [AttrResult].
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
