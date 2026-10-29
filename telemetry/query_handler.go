package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// QueryTelemetry returns an [eventsourcing.QueryHandlerMiddleware] that
// instruments every query dispatched through the bus with OpenTelemetry
// tracing and metrics, as [WithQueryTelemetry] describes. Use it with
// [eventsourcing.QueryBus.Use] to apply telemetry to all registered
// handlers; the query type is resolved at dispatch time from the query.
func QueryTelemetry(options ...Option) eventsourcing.QueryHandlerMiddleware {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}

	return func(next eventsourcing.QueryGateway[eventsourcing.Query, any]) eventsourcing.QueryGateway[eventsourcing.Query, any] {
		return func(ctx context.Context, qry eventsourcing.Query) (any, error) {
			return handleQuery(ctx, cfg, queryTypeName(qry), qry, func(ctx context.Context) (any, error) {
				return next(ctx, qry)
			})
		}
	}
}

// WithQueryTelemetry wraps next with OpenTelemetry tracing and metrics,
// returning an [eventsourcing.QueryHandler] of the same type that can be
// used as a drop-in replacement.
//
// Each call to the returned handler's HandleQuery starts a span named
// "handle" followed by the query type, such as "handle queries.GetTask",
// covering the full invocation of next and tagged with the query type and
// query ID. It also records [QueryActive] (in-flight queries) and
// [QueryDuration] labelled by [AttrResult].
//
//	handler := WithQueryTelemetry(myQueryHandler)
//	result, err := handler.HandleQuery(ctx, myQuery)
func WithQueryTelemetry[T eventsourcing.Query, R any](next eventsourcing.QueryHandler[T, R], options ...Option) eventsourcing.QueryHandler[T, R] {
	var zero T
	cfg := &config{}

	for _, o := range options {
		o.apply(cfg)
	}
	return &telemetryQueryHandler[T, R]{
		next:      next,
		queryType: queryTypeName(zero),
		cfg:       cfg,
	}
}

type telemetryQueryHandler[T eventsourcing.Query, R any] struct {
	next      eventsourcing.QueryHandler[T, R]
	queryType string
	cfg       *config
}

func (h *telemetryQueryHandler[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	var result R
	_, err := handleQuery(ctx, h.cfg, h.queryType, qry, func(ctx context.Context) (any, error) {
		var err error
		result, err = h.next.HandleQuery(ctx, qry)
		return result, err
	})
	return result, err
}

// queryTypeName returns the name a query's type is reported under: its Go
// type without a leading pointer, such as "queries.GetTask".
func queryTypeName(qry any) string {
	return strings.TrimPrefix(fmt.Sprintf("%T", qry), "*")
}

// handleQuery runs next under a span and records its outcome, for both
// [QueryTelemetry] and [WithQueryTelemetry].
func handleQuery(ctx context.Context, cfg *config, queryType string, qry eventsourcing.Query, next func(context.Context) (any, error)) (any, error) {
	attr := []attribute.KeyValue{
		AttrQueryType.String(queryType),
		AttrQueryID.String(string(qry.ID())),
	}
	attr = append(attr, cfg.Attributes...)
	if cfg.GetAttributes != nil {
		attr = append(attr, cfg.GetAttributes(ctx)...)
	}

	operation := "handle " + queryType
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
		trace.WithAttributes(attr...),
	)
	defer span.End()

	typeAttr := AttrQueryType.String(queryType)
	QueryActive.Add(ctx, 1, metric.WithAttributes(typeAttr))
	defer QueryActive.Add(ctx, -1, metric.WithAttributes(typeAttr))

	startTime := time.Now()
	result, err := next(ctx)
	duration := time.Since(startTime).Seconds()

	outcome := "success"
	if err != nil {
		outcome = "failure"
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	QueryDuration.Record(ctx, duration, metric.WithAttributes(typeAttr, AttrResult.String(outcome)))
	return result, err
}
