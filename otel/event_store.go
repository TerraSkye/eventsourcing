package otel

import (
	"context"
	"io"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var _ eventsourcing.EventStore = (*TelemetryStore)(nil)

type TelemetryStore struct {
	next eventsourcing.EventStore
}

// Save with metrics + span
func (t TelemetryStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	ctx, span := tracer.Start(ctx, "EventStore.Save",
		trace.WithAttributes(AttrOperation.String("save")),
	)
	defer span.End()

	var streamID string
	for _, event := range events {
		streamID = event.StreamID
		break
	}

	start := time.Now()
	result, err := t.next.Save(ctx, events, revision)
	duration := time.Since(start)

	EventStoreDuration.Record(ctx, float64(duration.Milliseconds()),
		metric.WithAttributes(AttrOperation.String("save"), AttrStreamID.String(streamID)),
	)
	EventStoreSaves.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(streamID)))
	EventsAppended.Add(ctx, int64(len(events)), metric.WithAttributes(AttrStreamID.String(streamID)))

	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(streamID)))
		span.RecordError(err)
	}

	return result, err
}

// LoadStream with inline tracing middleware
func (t TelemetryStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStream(ctx, id)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))
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
				trace.WithAttributes(AttrStreamID.String(id)),
			)
		}

		if !iter.Next(ctx) {
			err := iter.Err()
			if err == nil || err == io.EOF {
				EventStoreDuration.Record(ctx, float64(time.Since(startedAt).Milliseconds()), metric.WithAttributes(AttrStreamID.String(id)))
				rebuildSpan.End()
				if err == io.EOF {
					return nil, io.EOF
				}
			} else {
				EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))
				if rebuildSpan != nil {
					rebuildSpan.RecordError(err)
					rebuildSpan.End()
				}
			}
			return nil, err
		}

		val := iter.Value()
		EventsLoaded.Add(ctx, 1,
			metric.WithAttributes(
				AttrStreamID.String(id),
				AttrEventID.String(val.EventID.String()),
				AttrEventType.String(val.Event.EventType()),
				AttrEventStreamPos.Int64(int64(val.Version)),
			),
		)

		return val, nil
	}), nil
}

// LoadStreamFrom with inline tracing middleware
func (t TelemetryStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	iter, err := t.next.LoadStreamFrom(ctx, id, version)
	if err != nil {
		EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))
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
				trace.WithAttributes(AttrStreamID.String(id)),
			)
		}

		if !iter.Next(ctx) {
			rebuildSpan.SetAttributes(AttrEventCount.Int64(eventCount))
			if err := iter.Err(); err == nil {
				EventStoreDuration.Record(ctx, float64(time.Since(startedAt).Milliseconds()), metric.WithAttributes(AttrStreamID.String(id)))
			} else {
				EventStoreErrors.Add(ctx, 1, metric.WithAttributes(AttrStreamID.String(id)))
				rebuildSpan.RecordError(err)

			}
			rebuildSpan.End()
			return nil, io.EOF
		}

		eventCount++
		val := iter.Value()
		EventsLoaded.Add(ctx, 1,
			metric.WithAttributes(
				AttrStreamID.String(id),
				AttrEventID.String(val.EventID.String()),
				AttrEventType.String(val.Event.EventType()),
				AttrEventStreamPos.Int64(int64(val.Version)),
			),
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
			ctx, rebuildSpan = tracer.Start(ctx, "EventStore.LoadFromAll")
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
func WithEventStoreTelemetry(next eventsourcing.EventStore) eventsourcing.EventStore {
	return TelemetryStore{next: next}
}
