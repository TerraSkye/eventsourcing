package otel

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var _ eventsourcing.EventStore = (*TelemetryStore)(nil)

type TelemetryStore struct {
	next eventsourcing.EventStore
}

// Save with metrics + span
func (t TelemetryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	var streamID string
	for _, event := range events {
		streamID = event.StreamID
		break
	}

	ctx, span := tracer.Start(ctx, "EventStore.Save",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			AttrOperation.String("save"),
			AttrStreamID.String(streamID),
			AttrEventStreamPos.String(fmt.Sprintf("%T", revision)),
		),
	)
	defer span.End()

	{
		carrier := propagation.MapCarrier{}

		causationId := eventsourcing.CausationFromContext(ctx)

		otel.GetTextMapPropagator().Inject(ctx, carrier)
		for i := range events {
			if causationId != "" {
				events[i].Metadata["causationId"] = causationId
			}

			if span.SpanContext().HasTraceID() {
				events[i].Metadata["correlationId"] = span.SpanContext().TraceID().String()
			}

			for key, value := range carrier {
				events[i].Metadata[key] = value
			}
		}
	}

	start := time.Now()
	result, err := t.next.Save(ctx, events, revision)
	duration := time.Since(start)

	EventStoreDuration.Record(ctx, float64(duration.Milliseconds()),
		metric.WithAttributes(
			AttrOperation.String("save"),
		),
	)
	EventStoreSaves.Add(ctx, 1, metric.WithAttributes())
	EventsAppended.Add(ctx, int64(len(events)), metric.WithAttributes())

	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes())
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	return result, err
}

// LoadStream with inline tracing middleware
func (t TelemetryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStream(ctx, id)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes())
		return iter, err
	}

	started := false
	var startedAt time.Time
	var rebuildSpan trace.Span

	return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !started {
			started = true
			startedAt = time.Now()
			ctx, rebuildSpan = tracer.Start(ctx, "EventStore.LoadStream",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(AttrStreamID.String(id)),
			)
		}

		if !iter.Next(ctx) {
			err := iter.Err()
			if err == nil || err == io.EOF {
				EventStoreDuration.Record(ctx, float64(time.Since(startedAt).Milliseconds()), metric.WithAttributes())
				rebuildSpan.End()
				if err == io.EOF {
					return nil, io.EOF
				}
			} else {
				EventStoreErrors.Add(ctx, 1, metric.WithAttributes())
				if rebuildSpan != nil {
					rebuildSpan.RecordError(err)
					rebuildSpan.SetStatus(codes.Error, err.Error())
					rebuildSpan.End()
				}
			}
			return nil, err
		}

		val := iter.Value()
		EventsLoaded.Add(ctx, 1,
			metric.WithAttributes(),
		)

		return val, nil
	}), nil
}

// LoadStreamFrom with inline tracing middleware
func (t TelemetryStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStreamFrom(ctx, id, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes())
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
			ctx, rebuildSpan = tracer.Start(ctx, "EventStore.LoadStreamFrom",
				trace.WithSpanKind(trace.SpanKindClient),
				trace.WithAttributes(AttrStreamID.String(id)),
			)
		}

		if !iter.Next(ctx) {
			rebuildSpan.SetAttributes(AttrEventCount.Int64(eventCount))

			err := iter.Err()

			if err == nil {
				EventStoreDuration.Record(ctx, float64(time.Since(startedAt).Milliseconds()), metric.WithAttributes())
				rebuildSpan.End()
				return nil, io.EOF
			}

			EventStoreErrors.Add(ctx, 1, metric.WithAttributes())
			rebuildSpan.RecordError(err)
			rebuildSpan.SetStatus(codes.Error, err.Error())
			rebuildSpan.End()
			return nil, err
		}

		eventCount++
		val := iter.Value()
		EventsLoaded.Add(ctx, 1,
			metric.WithAttributes(),
		)

		return val, nil
	}), nil
}

// LoadFromAll with inline tracing middleware
func (t TelemetryStore) LoadFromAll(ctx context.Context, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadFromAll(ctx, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1)
		return iter, err
	}

	started := false
	var startedAt time.Time
	var rebuildSpan trace.Span

	return eventsourcing.NewIteratorFunc(func(ctx context.Context) (*eventsourcing.Envelope, error) {
		if !started {
			started = true
			startedAt = time.Now()
			ctx, rebuildSpan = tracer.Start(ctx, "EventStore.LoadFromAll",
				trace.WithSpanKind(trace.SpanKindClient),
			)
		}

		if !iter.Next(ctx) {
			err := iter.Err()
			if err == nil || err == io.EOF {
				EventStoreDuration.Record(ctx, float64(time.Since(startedAt).Milliseconds()))
				if rebuildSpan != nil {
					rebuildSpan.End()
				}
				if err == io.EOF {
					return nil, io.EOF
				}
			} else {
				EventStoreErrors.Add(ctx, 1)
				if rebuildSpan != nil {
					rebuildSpan.RecordError(err)
					rebuildSpan.SetStatus(codes.Error, err.Error())
					rebuildSpan.End()
				}
			}
			return nil, err
		}

		val := iter.Value()
		EventsLoaded.Add(ctx, 1)

		return val, nil
	}), nil
}

// Close just forwards
func (t TelemetryStore) Close() error {
	return t.next.Close()
}

// Constructor
func WithEventStoreTelemetry(next eventsourcing.EventStore, options ...Option) eventsourcing.EventStore {
	return TelemetryStore{next: next}
}
