package otel

import (
	"context"
	"io"
	"maps"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var _ eventsourcing.EventStore = (*TelemetryStore)(nil)

// TelemetryStore wraps an [eventsourcing.EventStore], instrumenting every
// save and load operation with OpenTelemetry tracing and metrics and
// injecting trace propagation headers into appended events' metadata.
// Construct one with [WithEventStoreTelemetry].
type TelemetryStore struct {
	next eventsourcing.EventStore
	cfg  *config
}

// baseAttrs returns the span attributes common to every operation: the
// configured db.system attribute (defaulting to "eventsourcing") followed by
// any other attributes from cfg.
func (t TelemetryStore) baseAttrs() []attribute.KeyValue {
	dbSystem := "eventsourcing"
	for _, kv := range t.cfg.Attributes {
		if string(kv.Key) == string(AttrDBSystem) {
			dbSystem = kv.Value.AsString()
		}
	}
	attrs := []attribute.KeyValue{AttrDBSystem.String(dbSystem)}
	for _, kv := range t.cfg.Attributes {
		if string(kv.Key) != string(AttrDBSystem) {
			attrs = append(attrs, kv)
		}
	}
	return attrs
}

// Save appends events to the underlying [eventsourcing.EventStore] inside a
// client span tagged with the stream ID and requested revision. Before
// saving, it stamps each event's Metadata with the current trace's
// causation ID (from [eventsourcing.CausationFromContext]), correlation ID
// (the trace ID, if the span has one), and injected trace-propagation
// headers, so a later [TelemetryEventBus] or event handler can link back to
// this trace. It records [EventStoreDuration], [EventStoreSaves], and
// [EventsAppended] for every call, and [EventStoreErrors] if the underlying
// store returns an error.
func (t TelemetryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	var streamID string
	for _, event := range events {
		streamID = event.StreamID
		break
	}

	spanAttrs := append(t.baseAttrs(),
		AttrOperation.String("save"),
		AttrStreamID.String(streamID),
		AttrStreamVersion.Int64(revision.ToRawInt64()),
	)

	spanName := "append eventstore"
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

		causationId := eventsourcing.CausationFromContext(ctx)

		otel.GetTextMapPropagator().Inject(ctx, carrier)
		for i := range events {

			md := make(map[string]any, len(events[i].Metadata)+len(carrier)+2)
			maps.Copy(md, events[i].Metadata)
			events[i].Metadata = md

			if causationId != "" {
				events[i].Metadata["causation_id"] = causationId
			}

			if span.SpanContext().HasTraceID() {
				events[i].Metadata["correlation_id"] = span.SpanContext().TraceID().String()
			}

			for key, value := range carrier {
				events[i].Metadata[key] = value
			}
		}
	}

	start := time.Now()
	result, err := t.next.Save(ctx, events, revision)
	duration := time.Since(start)

	EventStoreDuration.Record(ctx, duration.Seconds(),
		metric.WithAttributes(AttrOperation.String("save")),
	)
	EventStoreSaves.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("save")))
	EventsAppended.Add(ctx, int64(len(events)), metric.WithAttributes(AttrStreamID.String(streamID)))

	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("save")))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	return result, err
}

// LoadStream loads the stream id from the underlying
// [eventsourcing.EventStore]. If the initial call fails, it records
// [EventStoreErrors] and returns immediately. Otherwise it returns an
// iterator that, on its first advance, starts a client span tagged with the
// stream ID; each yielded event increments [EventsLoaded].
//
// The returned iterator owns the underlying one: closing it — by reading to
// the end, by failing, or by an explicit Close — closes the underlying
// iterator, ends the span, and records [EventStoreDuration], or
// [EventStoreErrors] if the load failed. A caller that stops early is
// therefore instrumented like any other, provided it closes the iterator.
func (t TelemetryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	src, err := t.next.LoadStream(ctx, id)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
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
					AttrOperation.String("load"),
					AttrStreamID.String(id),
				)
				// Keep the span's context for the metrics recorded below, so
				// they are attributed to the span rather than to ctx alone.
				spanCtx, span = tracer.Start(ctx, "load eventstore",
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

			EventsLoaded.Add(spanCtx, 1, metric.WithAttributes(AttrStreamID.String(id)))
			return src.Value(), nil
		},
		func(err error) {
			if span == nil {
				// Closed before the first advance: no span was started, so
				// there is no load to report.
				return
			}
			if err != nil {
				EventStoreErrors.Add(spanCtx, 1, metric.WithAttributes(AttrOperation.String("load")))
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				EventStoreDuration.Record(spanCtx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
			}
			span.End()
		}), nil
}

// LoadStreamFrom loads stream id from version onward from the underlying
// [eventsourcing.EventStore]. It behaves like [TelemetryStore.LoadStream],
// additionally tagging the span with the requested version and, once the
// iterator closes, with the number of events yielded.
func (t TelemetryStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	src, err := t.next.LoadStreamFrom(ctx, id, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
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
					AttrOperation.String("load"),
					AttrStreamID.String(id),
					AttrStreamVersion.Int64(version.ToRawInt64()),
				)
				spanCtx, span = tracer.Start(ctx, "load eventstore",
					trace.WithSpanKind(trace.SpanKindClient),
					trace.WithAttributes(spanAttrs...),
				)
			}

			if !src.Next() {
				return nil, io.EOF
			}

			eventCount++
			EventsLoaded.Add(spanCtx, 1, metric.WithAttributes(AttrStreamID.String(id)))
			return src.Value(), nil
		},
		func(err error) {
			if span == nil {
				return
			}
			span.SetAttributes(AttrEventCount.Int64(eventCount))
			if err != nil {
				EventStoreErrors.Add(spanCtx, 1, metric.WithAttributes(AttrOperation.String("load")))
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				EventStoreDuration.Record(spanCtx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
			}
			span.End()
		}), nil
}

// LoadFromAll loads all events across streams from version onward from the
// underlying [eventsourcing.EventStore]. It behaves like
// [TelemetryStore.LoadStream], tagging the span with the requested version
// instead of a stream ID, since the events may belong to any stream.
func (t TelemetryStore) LoadFromAll(ctx context.Context, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	src, err := t.next.LoadFromAll(ctx, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
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
					AttrOperation.String("load"),
					AttrStreamVersion.Int64(version.ToRawInt64()),
				)
				spanCtx, span = tracer.Start(ctx, "load eventstore",
					trace.WithSpanKind(trace.SpanKindClient),
					trace.WithAttributes(spanAttrs...),
				)
			}

			if !src.Next() {
				return nil, io.EOF
			}

			EventsLoaded.Add(spanCtx, 1)
			return src.Value(), nil
		},
		func(err error) {
			if span == nil {
				return
			}
			if err != nil {
				EventStoreErrors.Add(spanCtx, 1, metric.WithAttributes(AttrOperation.String("load")))
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				EventStoreDuration.Record(spanCtx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
			}
			span.End()
		}), nil
}

// Close closes the underlying [eventsourcing.EventStore]. It delegates
// directly, without additional instrumentation.
func (t TelemetryStore) Close() error {
	return t.next.Close()
}

// WithEventStoreTelemetry wraps next in a [TelemetryStore], instrumenting
// every save and load with OpenTelemetry tracing and metrics as described on
// [TelemetryStore]. Options such as [WithAttributes] and [WithOperation]
// customize the spans produced.
func WithEventStoreTelemetry(next eventsourcing.EventStore, options ...Option) eventsourcing.EventStore {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	return TelemetryStore{next: next, cfg: cfg}
}

// EventStoreTelemetry returns an [eventsourcing.EventStoreMiddleware] that
// instruments an [eventsourcing.EventStore] with OpenTelemetry tracing and
// metrics; it wraps the store with [WithEventStoreTelemetry].
//
//	store = otel.EventStoreTelemetry()(store)
func EventStoreTelemetry(options ...Option) eventsourcing.EventStoreMiddleware {
	return func(next eventsourcing.EventStore) eventsourcing.EventStore {
		return WithEventStoreTelemetry(next, options...)
	}
}
