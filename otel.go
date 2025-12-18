package eventsourcing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/terraskye/eventsourcing"
)

// Semantic attribute keys following OpenTelemetry conventions
const (
	// Command attributes
	AttrCommandType   = attribute.Key("eventsourcing.command.type")
	AttrCommandID     = attribute.Key("eventsourcing.command.id")
	AttrAggregateType = attribute.Key("eventsourcing.aggregate.type")
	AttrAggregateID   = attribute.Key("eventsourcing.aggregate.id")

	// Stream attributes
	AttrStreamID      = attribute.Key("eventsourcing.stream.id")
	AttrStreamVersion = attribute.Key("eventsourcing.stream.version")
	AttrRevisionType  = attribute.Key("eventsourcing.stream.revision_type")

	// Event attributes
	AttrEventType      = attribute.Key("eventsourcing.event.type")
	AttrEventID        = attribute.Key("eventsourcing.event.id")
	AttrEventCount     = attribute.Key("eventsourcing.events.count")
	AttrEventGlobalPos = attribute.Key("eventsourcing.event.global_position")
	AttrEventStreamPos = attribute.Key("eventsourcing.event.stream_position")

	// Query attributes
	AttrQueryType   = attribute.Key("eventsourcing.query.type")
	AttrQueryID     = attribute.Key("eventsourcing.query.id")
	AttrResultType  = attribute.Key("eventsourcing.query.result_type")
	AttrResultCount = attribute.Key("eventsourcing.query.result_count")

	// EventBus attributes
	AttrSubscriberName  = attribute.Key("eventsourcing.subscriber.name")
	AttrSubscriberCount = attribute.Key("eventsourcing.subscriber.count")
	AttrHandlerName     = attribute.Key("eventsourcing.handler.name")

	// Error attributes
	AttrErrorType    = attribute.Key("eventsourcing.error.type")
	AttrErrorMessage = attribute.Key("eventsourcing.error.message")
	AttrRetryCount   = attribute.Key("eventsourcing.retry.count")
	AttrRetryMax     = attribute.Key("eventsourcing.retry.max")

	// Operation attributes
	AttrOperation    = attribute.Key("eventsourcing.operation")
	AttrConflictType = attribute.Key("eventsourcing.conflict.type")
	AttrShardID      = attribute.Key("eventsourcing.shard.id")
	AttrQueueDepth   = attribute.Key("eventsourcing.queue.depth")
)

var (
	meter  = otel.Meter(instrumentationName, metric.WithInstrumentationVersion(instrumentationVersion))
	tracer = otel.Tracer(instrumentationName, trace.WithInstrumentationVersion(instrumentationVersion))

	// Command metrics
	CommandsHandled, _ = meter.Int64Counter(
		"eventsourcing.commands.handled",
		metric.WithDescription("Total number of commands handled"),
		metric.WithUnit("{command}"),
	)

	CommandsDuration, _ = meter.Float64Histogram(
		"eventsourcing.commands.duration",
		metric.WithDescription("Command handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000),
	)

	CommandsInFlight, _ = meter.Int64UpDownCounter(
		"eventsourcing.commands.in_flight",
		metric.WithDescription("Number of commands currently being processed"),
		metric.WithUnit("{command}"),
	)

	CommandsRetried, _ = meter.Int64Counter(
		"eventsourcing.commands.retried",
		metric.WithDescription("Number of command retries due to conflicts"),
		metric.WithUnit("{retry}"),
	)

	CommandsFailed, _ = meter.Int64Counter(
		"eventsourcing.commands.failed",
		metric.WithDescription("Number of failed commands"),
		metric.WithUnit("{command}"),
	)

	// Event metrics
	EventsAppended, _ = meter.Int64Counter(
		"eventsourcing.events.appended",
		metric.WithDescription("Number of events appended to streams"),
		metric.WithUnit("{event}"),
	)

	EventsLoaded, _ = meter.Int64Counter(
		"eventsourcing.events.loaded",
		metric.WithDescription("Number of events loaded from streams"),
		metric.WithUnit("{event}"),
	)

	EventsPublished, _ = meter.Int64Counter(
		"eventsourcing.events.published",
		metric.WithDescription("Number of events published to event bus"),
		metric.WithUnit("{event}"),
	)

	// EventBus metrics
	EventBusPublished, _ = meter.Int64Counter(
		"eventsourcing.eventbus.published",
		metric.WithDescription("Number of events published to event bus"),
		metric.WithUnit("{event}"),
	)

	EventBusHandled, _ = meter.Int64Counter(
		"eventsourcing.eventbus.handled",
		metric.WithDescription("Number of events handled by subscribers"),
		metric.WithUnit("{event}"),
	)

	EventBusErrors, _ = meter.Int64Counter(
		"eventsourcing.eventbus.errors",
		metric.WithDescription("Number of event bus handler errors"),
		metric.WithUnit("{error}"),
	)

	EventBusSubscribers, _ = meter.Int64UpDownCounter(
		"eventsourcing.eventbus.subscribers",
		metric.WithDescription("Number of active event bus subscribers"),
		metric.WithUnit("{subscriber}"),
	)

	EventBusDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventbus.duration",
		metric.WithDescription("Event bus handler duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000),
	)

	// Query metrics
	QueriesHandled, _ = meter.Int64Counter(
		"eventsourcing.queries.handled",
		metric.WithDescription("Total number of queries handled"),
		metric.WithUnit("{query}"),
	)

	QueriesDuration, _ = meter.Float64Histogram(
		"eventsourcing.queries.duration",
		metric.WithDescription("Query handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000),
	)

	QueriesInFlight, _ = meter.Int64UpDownCounter(
		"eventsourcing.queries.in_flight",
		metric.WithDescription("Number of queries currently being processed"),
		metric.WithUnit("{query}"),
	)

	QueriesFailed, _ = meter.Int64Counter(
		"eventsourcing.queries.failed",
		metric.WithDescription("Number of failed queries"),
		metric.WithUnit("{query}"),
	)

	// EventStore metrics
	EventStoreSaves, _ = meter.Int64Counter(
		"eventsourcing.eventstore.saves",
		metric.WithDescription("Number of save operations"),
		metric.WithUnit("{operation}"),
	)

	EventStoreLoads, _ = meter.Int64Counter(
		"eventsourcing.eventstore.loads",
		metric.WithDescription("Number of load operations"),
		metric.WithUnit("{operation}"),
	)

	EventStoreDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventstore.duration",
		metric.WithDescription("Event store operation duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000),
	)

	EventStoreErrors, _ = meter.Int64Counter(
		"eventsourcing.eventstore.errors",
		metric.WithDescription("Number of event store errors"),
		metric.WithUnit("{error}"),
	)

	// System metrics
	ConcurrencyConflicts, _ = meter.Int64Counter(
		"eventsourcing.concurrency.conflicts",
		metric.WithDescription("Number of concurrency conflicts"),
		metric.WithUnit("{conflict}"),
	)

	StreamVersionGauge, _ = meter.Int64Gauge(
		"eventsourcing.stream.version",
		metric.WithDescription("Current version of streams"),
		metric.WithUnit("{version}"),
	)

	CommandBusQueueDepth, _ = meter.Int64UpDownCounter(
		"eventsourcing.commandbus.queue_depth",
		metric.WithDescription("Current depth of command bus queues"),
		metric.WithUnit("{command}"),
	)
)

// Helper functions for creating spans and recording metrics

// StartCommandSpan starts a new span for command handling
func StartCommandSpan(ctx context.Context, cmd Command) (context.Context, trace.Span) {
	commandType := TypeName(cmd)
	ctx, span := tracer.Start(ctx, fmt.Sprintf("command.handle %s", commandType),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrCommandType.String(commandType),
			AttrAggregateID.String(cmd.AggregateID()),
		),
	)
	return ctx, span
}

// EndCommandSpan ends a command span with success/failure
func EndCommandSpan(span trace.Span, result *AppendResult, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, "")
		if result != nil {
			span.SetAttributes(
				AttrStreamVersion.Int64(int64(result.NextExpectedVersion)),
			)
		}
	}
	span.End()
}

// RecordCommandRetry records a retry attempt
func RecordCommandRetry(ctx context.Context, span trace.Span, attempt int, maxRetries int) {
	span.AddEvent("command.retry",
		trace.WithAttributes(
			AttrRetryCount.Int(attempt),
			AttrRetryMax.Int(maxRetries),
		),
	)

	if CommandsRetried != nil {
		CommandsRetried.Add(ctx, 1,
			metric.WithAttributes(
				AttrRetryCount.Int(attempt),
			),
		)
	}
}

// StartEventStoreSpan starts a span for event store operations
func StartEventStoreSpan(ctx context.Context, operation string, streamID string) (context.Context, trace.Span) {
	ctx, span := tracer.Start(ctx, fmt.Sprintf("eventstore.%s", operation),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			AttrOperation.String(operation),
			AttrStreamID.String(streamID),
		),
	)
	return ctx, span
}

// EndEventStoreSpan ends an event store span
func EndEventStoreSpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// RecordConcurrencyConflict records a concurrency conflict
func RecordConcurrencyConflict(ctx context.Context, span trace.Span, streamID string, expectedVersion, actualVersion uint64) {
	span.AddEvent("concurrency.conflict",
		trace.WithAttributes(
			AttrStreamID.String(streamID),
			attribute.Int64("expected_version", int64(expectedVersion)),
			attribute.Int64("actual_version", int64(actualVersion)),
		),
	)

	if ConcurrencyConflicts != nil {
		ConcurrencyConflicts.Add(ctx, 1,
			metric.WithAttributes(
				AttrStreamID.String(streamID),
			),
		)
	}
}

// StartQuerySpan starts a span for query handling
func StartQuerySpan(ctx context.Context, qry Query) (context.Context, trace.Span) {
	queryType := TypeName(qry)
	ctx, span := tracer.Start(ctx, fmt.Sprintf("query.handle %s", queryType),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			AttrQueryType.String(queryType),
		),
	)
	return ctx, span
}

// EndQuerySpan ends a query span
func EndQuerySpan(span trace.Span, err error) {
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		span.RecordError(err)
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// StartEventHandlerSpan starts a span for event handling
func StartEventHandlerSpan(ctx context.Context, event Event, handlerName string) (context.Context, trace.Span) {
	eventType := event.EventType()
	ctx, span := tracer.Start(ctx, fmt.Sprintf("event.handle %s", eventType),
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			AttrEventType.String(eventType),
			AttrHandlerName.String(handlerName),
			AttrAggregateID.String(event.AggregateID()),
		),
	)

	// Link to the event's trace if available
	if envelope := ctx.Value(eventIDKey); envelope != nil {
		if env, ok := envelope.(*Envelope); ok {
			span.SetAttributes(
				AttrEventID.String(env.EventID.String()),
				AttrStreamVersion.Int64(int64(env.Version)),
			)
		}
	}

	return ctx, span
}

// EndEventHandlerSpan ends an event handler span
func EndEventHandlerSpan(span trace.Span, err error) {
	if err != nil {
		if _, isSkipped := err.(ErrSkippedEvent); isSkipped {
			span.SetStatus(codes.Ok, "event skipped")
		} else {
			span.SetStatus(codes.Error, err.Error())
			span.RecordError(err)
		}
	} else {
		span.SetStatus(codes.Ok, "")
	}
	span.End()
}

// RecordEventsAppended records metrics when events are appended
func RecordEventsAppended(ctx context.Context, streamID string, eventCount int, revision StreamState) {
	if EventsAppended != nil {
		attrs := []attribute.KeyValue{
			AttrStreamID.String(streamID),
			AttrEventCount.Int(eventCount),
		}

		// Add revision type for better filtering
		switch revision.(type) {
		case Any:
			attrs = append(attrs, AttrRevisionType.String("any"))
		case NoStream:
			attrs = append(attrs, AttrRevisionType.String("no_stream"))
		case StreamExists:
			attrs = append(attrs, AttrRevisionType.String("stream_exists"))
		case Revision:
			attrs = append(attrs, AttrRevisionType.String("explicit"))
		}

		EventsAppended.Add(ctx, int64(eventCount), metric.WithAttributes(attrs...))
	}
}

// RecordEventsLoaded records metrics when events are loaded
func RecordEventsLoaded(ctx context.Context, streamID string, eventCount int) {
	if EventsLoaded != nil {
		EventsLoaded.Add(ctx, int64(eventCount),
			metric.WithAttributes(
				AttrStreamID.String(streamID),
				AttrEventCount.Int(eventCount),
			),
		)
	}
}
