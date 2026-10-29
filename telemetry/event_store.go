package telemetry

import (
	"context"
	"io"
	"maps"
	"math"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ eventsourcing.EventStore   = EventStore{}
	_ eventsourcing.EventStore   = telemetryGlobalStore{}
	_ eventsourcing.GlobalReader = telemetryGlobalStore{}
)

// EventStore wraps an [eventsourcing.EventStore], instrumenting every
// save and load operation with OpenTelemetry tracing and metrics and
// injecting trace propagation headers into appended events' metadata.
// Construct one with [WithEventStoreTelemetry].
type EventStore struct {
	next eventsourcing.EventStore
	cfg  *config
}

// baseAttrs returns the span attributes common to every operation: the
// attributes from cfg, such as [AttrDBSystemName].
func (t EventStore) baseAttrs() []attribute.KeyValue {
	return append([]attribute.KeyValue(nil), t.cfg.Attributes...)
}

// metricAttrs returns the attributes [EventStoreDuration] is recorded with
// for operation op that returned err, plus [AttrDBSystemName] when cfg sets
// it, so stores of different systems can be told apart.
func (t EventStore) metricAttrs(op string, err error) metric.MeasurementOption {
	attrs := []attribute.KeyValue{AttrEventStoreOperation.String(op), AttrResult.String("success")}
	if err != nil {
		attrs = []attribute.KeyValue{AttrEventStoreOperation.String(op), AttrResult.String("failure"), AttrErrorType.String(errorType(err))}
	}
	return metric.WithAttributes(append(attrs, t.dbSystem()...)...)
}

// dbSystem returns the [AttrDBSystemName] attribute from cfg, if it sets one.
// Only [WithAttributes] counts: an attribute from [WithAttributeGetter] is
// computed per call for spans and does not reach the metrics.
func (t EventStore) dbSystem() []attribute.KeyValue {
	for _, kv := range t.cfg.Attributes {
		if kv.Key == AttrDBSystemName {
			return []attribute.KeyValue{kv}
		}
	}
	return nil
}

// Save appends events to the underlying [eventsourcing.EventStore] inside a
// client span tagged with the stream ID, the requested revision, and the
// events' correlation and causation IDs. Before saving, it stamps each
// event's Metadata with injected trace-propagation headers, so a later
// event handler can link back to this trace. It records [EventStoreDuration]
// for every call, labelled by [AttrResult], and counts the events in
// [EventStoreAppended] when the save succeeds.
func (t EventStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	var streamID string
	for _, event := range events {
		streamID = event.StreamID
		break
	}

	spanAttrs := append(t.baseAttrs(),
		AttrEventStoreOperation.String("append"),
		AttrStreamID.String(streamID),
		AttrStreamVersion.Int64(revision.ToRawInt64()),
	)
	if len(events) > 0 {
		spanAttrs = append(spanAttrs,
			AttrCorrelationID.String(events[0].CorrelationID),
			AttrCausationID.String(events[0].CausationID),
		)
	}

	spanName := "append events"
	if t.cfg.Operation != "" {
		spanName = t.cfg.Operation
	}
	if t.cfg.GetOperation != nil {
		if op := t.cfg.GetOperation(ctx, spanName); op != "" {
			spanName = op
		}
	}

	ctx, span := tracer.Start(ctx, spanName,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttrs...),
	)
	defer span.End()

	{
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(ctx, carrier)
		for i := range events {
			md := make(map[string]any, len(events[i].Metadata)+len(carrier))
			maps.Copy(md, events[i].Metadata)
			events[i].Metadata = md

			for key, value := range carrier {
				events[i].Metadata[key] = value
			}
		}
	}

	start := time.Now()
	result, err := t.next.Save(ctx, events, revision)
	duration := time.Since(start)

	EventStoreDuration.Record(ctx, duration.Seconds(), t.metricAttrs("append", err))

	if err == nil {
		EventStoreAppended.Add(ctx, int64(len(events)), metric.WithAttributes(t.dbSystem()...))
	} else {
		span.SetAttributes(AttrErrorType.String(errorType(err)))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	return result, err
}

// LoadStream loads the stream id from the underlying
// [eventsourcing.EventStore]. If the initial call fails, it records
// [EventStoreDuration] with result "failure" and returns immediately.
// Otherwise it returns an iterator that, on its first advance, starts a
// client span tagged with the stream ID; each yielded event increments
// [EventStoreLoaded].
//
// The returned iterator owns the underlying one: closing it — by reading to
// the end, by failing, or by an explicit Close — closes the underlying
// iterator, ends the span, and records [EventStoreDuration], labelled by
// whether the load failed. A caller that stops early is
// therefore instrumented like any other, provided it closes the iterator.
func (t EventStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	calledAt := time.Now()
	src, err := t.next.LoadStream(ctx, id)
	if err != nil {
		EventStoreDuration.Record(ctx, time.Since(calledAt).Seconds(), t.metricAttrs("load", err))
		return src, err
	}

	var (
		startedAt time.Time
		span      trace.Span
		spanCtx   = ctx
	)

	return eventsourcing.Wrap(src,
		func(ctx context.Context, src *eventsourcing.Iterator[*eventsourcing.Envelope]) (*eventsourcing.Envelope, error) {
			if span == nil {
				startedAt = time.Now()
				spanAttrs := append(t.baseAttrs(),
					AttrEventStoreOperation.String("load"),
					AttrStreamID.String(id),
				)
				// Keep the span's context for the metrics recorded below, so
				// they are attributed to the span rather than to ctx alone.
				spanCtx, span = tracer.Start(ctx, "load events",
					trace.WithSpanKind(trace.SpanKindClient),
					trace.WithAttributes(spanAttrs...),
				)
			}

			if !src.Next() {
				// io.EOF, not src.Err(): the source's failure reaches this
				// iterator through Close, and the done function below sees
				// it. Returning it here as well would report it twice.
				return nil, io.EOF
			}

			EventStoreLoaded.Add(spanCtx, 1, metric.WithAttributes(t.dbSystem()...))
			return src.Value(), nil
		},
		func(err error) {
			if span == nil {
				// Closed before the first advance: no span was started, so
				// there is no load to report.
				return
			}
			EventStoreDuration.Record(spanCtx, time.Since(startedAt).Seconds(), t.metricAttrs("load", err))
			if err != nil {
				span.SetAttributes(AttrErrorType.String(errorType(err)))
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}), nil
}

// LoadStreamFrom loads stream id from version onward from the underlying
// [eventsourcing.EventStore]. It behaves like [EventStore.LoadStream],
// additionally tagging the span with the requested version and, once the
// iterator closes, with the number of events yielded.
func (t EventStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	calledAt := time.Now()
	src, err := t.next.LoadStreamFrom(ctx, id, version)
	if err != nil {
		EventStoreDuration.Record(ctx, time.Since(calledAt).Seconds(), t.metricAttrs("load", err))
		return src, err
	}

	var (
		startedAt  time.Time
		span       trace.Span
		spanCtx    = ctx
		eventCount int64
	)

	return eventsourcing.Wrap(src,
		func(ctx context.Context, src *eventsourcing.Iterator[*eventsourcing.Envelope]) (*eventsourcing.Envelope, error) {
			if span == nil {
				startedAt = time.Now()
				spanAttrs := append(t.baseAttrs(),
					AttrEventStoreOperation.String("load"),
					AttrStreamID.String(id),
					AttrStreamVersion.Int64(version.ToRawInt64()),
				)
				spanCtx, span = tracer.Start(ctx, "load events",
					trace.WithSpanKind(trace.SpanKindClient),
					trace.WithAttributes(spanAttrs...),
				)
			}

			if !src.Next() {
				return nil, io.EOF
			}

			eventCount++
			EventStoreLoaded.Add(spanCtx, 1, metric.WithAttributes(t.dbSystem()...))
			return src.Value(), nil
		},
		func(err error) {
			if span == nil {
				return
			}
			span.SetAttributes(AttrEventCount.Int64(eventCount))
			EventStoreDuration.Record(spanCtx, time.Since(startedAt).Seconds(), t.metricAttrs("load", err))
			if err != nil {
				span.SetAttributes(AttrErrorType.String(errorType(err)))
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}), nil
}

// telemetryGlobalStore is an [EventStore] around a store that also
// implements [eventsourcing.GlobalReader]. [WithEventStoreTelemetry] returns
// one in that case, so the wrapper reads the global log exactly when the
// wrapped store can.
type telemetryGlobalStore struct {
	EventStore
	reader eventsourcing.GlobalReader
}

// ReadAll reads a page of the global log from the wrapped store and
// records [EventStoreDuration], labelled by [AttrResult], and
// [EventStoreLoaded].
//
// A read that returns events or fails is recorded as a client span, tagged
// with the after position and the number of events returned. A read that
// returns no events is not: a projection at the head of the log polls on
// every interval, and a span per poll would drown the reads that matter.
// The span is created once the read is done, so spans the wrapped store
// starts itself are not its children.
func (t telemetryGlobalStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	startedAt := time.Now()
	res, err := t.reader.ReadAll(ctx, after, opts)
	EventStoreDuration.Record(ctx, time.Since(startedAt).Seconds(), t.metricAttrs("read_all", err))
	if err == nil {
		EventStoreLoaded.Add(ctx, int64(len(res.Events)), metric.WithAttributes(t.dbSystem()...))
		if len(res.Events) == 0 {
			return res, nil
		}
	}

	_, span := tracer.Start(ctx, "read all events",
		trace.WithTimestamp(startedAt),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(append(t.baseAttrs(),
			AttrEventStoreOperation.String("read_all"),
			AttrStreamVersion.Int64(int64(min(after, math.MaxInt64))),
			AttrEventCount.Int64(int64(len(res.Events))),
		)...),
	)
	defer span.End()
	if err != nil {
		span.SetAttributes(AttrErrorType.String(errorType(err)))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return res, err
}

// Head returns the head of the global log from the wrapped store. It is not
// traced: it is a cheap call made often to measure lag.
func (t telemetryGlobalStore) Head(ctx context.Context) (uint64, error) {
	return t.reader.Head(ctx)
}

// Close closes the underlying [eventsourcing.EventStore]. It delegates
// directly, without additional instrumentation.
func (t EventStore) Close() error {
	return t.next.Close()
}

// WithEventStoreTelemetry wraps next in an [EventStore], instrumenting
// every save and load with OpenTelemetry tracing and metrics as described on
// [EventStore]. [WithAttributes] adds attributes to every span; set
// [AttrDBSystemName] with it to label the store's metrics too.
// [WithOperation] and [WithOperationGetter] rename the "append events" span.
//
// If next also implements [eventsourcing.GlobalReader], so does the returned
// store, and its ReadAll is traced as well; otherwise the returned store is
// only an [eventsourcing.EventStore].
func WithEventStoreTelemetry(next eventsourcing.EventStore, options ...Option) eventsourcing.EventStore {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	store := EventStore{next: next, cfg: cfg}
	if reader, ok := next.(eventsourcing.GlobalReader); ok {
		return telemetryGlobalStore{EventStore: store, reader: reader}
	}
	return store
}

// EventStoreTelemetry returns an [eventsourcing.EventStoreMiddleware] that
// instruments an [eventsourcing.EventStore] with OpenTelemetry tracing and
// metrics; it wraps the store with [WithEventStoreTelemetry].
//
//	store = telemetry.EventStoreTelemetry()(store)
func EventStoreTelemetry(options ...Option) eventsourcing.EventStoreMiddleware {
	return func(next eventsourcing.EventStore) eventsourcing.EventStore {
		return WithEventStoreTelemetry(next, options...)
	}
}
