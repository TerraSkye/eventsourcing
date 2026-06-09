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

type TelemetryStore struct {
	next eventsourcing.EventStore
	cfg  *config
}

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

// Save with metrics + span
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

// LoadStream with inline tracing middleware
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

// LoadStreamFrom with inline tracing middleware
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

// LoadFromAll with inline tracing middleware
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

// Close just forwards
func (t TelemetryStore) Close() error {
	return t.next.Close()
}

// WithEventStoreTelemetry wraps an EventStore with OpenTelemetry tracing and metrics.
func WithEventStoreTelemetry(next eventsourcing.EventStore, options ...Option) eventsourcing.EventStore {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	return TelemetryStore{next: next, cfg: cfg}
}

// EventStoreTelemetry returns an EventStoreMiddleware that instruments the store
// with OpenTelemetry tracing and metrics.
// Use with ApplyEventStoreMiddleware() to compose store middleware declaratively.
func EventStoreTelemetry(options ...Option) eventsourcing.EventStoreMiddleware {
	return func(next eventsourcing.EventStore) eventsourcing.EventStore {
		return WithEventStoreTelemetry(next, options...)
	}
}
