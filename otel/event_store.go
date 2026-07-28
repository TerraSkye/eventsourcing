package otel

import (
	"context"
	"io"
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

	ctx, span := tracer.Start(ctx, "append eventstore",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(spanAttrs...),
	)
	defer span.End()

	{
		carrier := propagation.MapCarrier{}

		causationId := eventsourcing.CausationFromContext(ctx)

		otel.GetTextMapPropagator().Inject(ctx, carrier)
		for i := range events {

			if events[i].Metadata == nil {
				events[i].Metadata = map[string]any{}
			}

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
// stream ID; each yielded event increments [EventsLoaded], and
// [EventStoreDuration] is recorded, and [EventStoreErrors] incremented on
// failure, once the iterator is exhausted.
func (t TelemetryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStream(ctx, id)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
		return iter, err
	}

	started := false
	var startedAt time.Time
	var rebuildSpan trace.Span

	return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !started {
			started = true
			startedAt = time.Now()

			spanAttrs := append(t.baseAttrs(),
				AttrOperation.String("load"),
				AttrStreamID.String(id),
			)
			ctx, rebuildSpan = tracer.Start(ctx, "load eventstore",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(spanAttrs...),
			)
		}

		if !iter.Next(ctx) {
			err := iter.Err()
			if err == nil || err == io.EOF {
				EventStoreDuration.Record(ctx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
				rebuildSpan.End()
				return nil, io.EOF
			} else {
				EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
				if rebuildSpan != nil {
					rebuildSpan.RecordError(err)
					rebuildSpan.SetStatus(codes.Error, err.Error())
					rebuildSpan.End()
				}
			}
			return nil, err
		}

		val := iter.Value()
		EventsLoaded.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))

		return val, nil
	}), nil
}

// LoadStreamFrom loads stream id from version onward from the underlying
// [eventsourcing.EventStore]. It behaves like [TelemetryStore.LoadStream],
// additionally tagging the span with the requested version and, once the
// iterator is exhausted, with the number of events yielded.
func (t TelemetryStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStreamFrom(ctx, id, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
		return iter, err
	}

	started := false
	var startedAt time.Time
	var rebuildSpan trace.Span
	var eventCount int64

	return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !started {
			started = true
			startedAt = time.Now()

			spanAttrs := append(t.baseAttrs(),
				AttrOperation.String("load"),
				AttrStreamID.String(id),
				AttrStreamVersion.Int64(version.ToRawInt64()),
			)
			ctx, rebuildSpan = tracer.Start(ctx, "load eventstore",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(spanAttrs...),
			)
		}

		if !iter.Next(ctx) {
			rebuildSpan.SetAttributes(AttrEventCount.Int64(eventCount))

			err := iter.Err()

			if err == nil {
				EventStoreDuration.Record(ctx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
				rebuildSpan.End()
				return nil, io.EOF
			}

			EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
			rebuildSpan.RecordError(err)
			rebuildSpan.SetStatus(codes.Error, err.Error())
			rebuildSpan.End()
			return nil, err
		}

		eventCount++
		val := iter.Value()
		EventsLoaded.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))

		return val, nil
	}), nil
}

// LoadFromAll loads all events across streams from version onward from the
// underlying [eventsourcing.EventStore]. It behaves like
// [TelemetryStore.LoadStream], tagging the span with the requested version
// instead of a stream ID, since the events may belong to any stream.
func (t TelemetryStore) LoadFromAll(ctx context.Context, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadFromAll(ctx, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
		return iter, err
	}

	started := false
	var startedAt time.Time
	var rebuildSpan trace.Span

	return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !started {
			started = true
			startedAt = time.Now()

			spanAttrs := append(t.baseAttrs(),
				AttrOperation.String("load"),
				AttrStreamVersion.Int64(version.ToRawInt64()),
			)
			ctx, rebuildSpan = tracer.Start(ctx, "load eventstore",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(spanAttrs...),
			)
		}

		if !iter.Next(ctx) {
			err := iter.Err()
			if err == nil || err == io.EOF {
				EventStoreDuration.Record(ctx, time.Since(startedAt).Seconds(), metric.WithAttributes(AttrOperation.String("load")))
				if rebuildSpan != nil {
					rebuildSpan.End()
				}
				if err == io.EOF {
					return nil, io.EOF
				}
			} else {
				EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrOperation.String("load")))
				if rebuildSpan != nil {
					rebuildSpan.RecordError(err)
					rebuildSpan.SetStatus(codes.Error, err.Error())
					rebuildSpan.End()
				}
			}
			return nil, err
		}

		val := iter.Value()
		EventsLoaded.Add(ctx, 1, metric.WithAttributes())

		return val, nil
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
