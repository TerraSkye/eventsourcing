package otel

import (
	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/terraskye/eventsourcing"
)

// Semantic attribute keys following OpenTelemetry conventions
const (
	// Command attributes
	AttrCommandType = attribute.Key("eventsourcing.command.type")
	AttrAggregateID = attribute.Key("eventsourcing.aggregate.id")

	// Stream attributes
	AttrStreamID      = attribute.Key("eventsourcing.stream.id")
	AttrStreamVersion = attribute.Key("eventsourcing.stream.version")

	// EventData attributes
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
	meter  = otel.Meter(instrumentationName, metric.WithInstrumentationVersion(eventsourcing.InstrumentationVersion))
	tracer = otel.Tracer(instrumentationName, trace.WithInstrumentationVersion(eventsourcing.InstrumentationVersion))

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

	CommandsFailed, _ = meter.Int64Counter(
		"eventsourcing.commands.failed",
		metric.WithDescription("Number of failed commands"),
		metric.WithUnit("{command}"),
	)

	// EventData metrics
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
		metric.WithDescription("EventData bus handler duration"),
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
		metric.WithDescription("EventData store operation duration"),
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
