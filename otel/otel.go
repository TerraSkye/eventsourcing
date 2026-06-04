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
	// Database attributes (OpenTelemetry semantic conventions)
	AttrDBSystem = attribute.Key("db.system")

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
	AttrSubscriberName = attribute.Key("eventsourcing.subscriber.name")
	AttrHandlerName    = attribute.Key("eventsourcing.handler.name")

	// Result attribute for outcome labelling on counters
	AttrResult = attribute.Key("eventsourcing.result") // "success" | "failure"

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
	CommandsCount, _ = meter.Int64Counter(
		"eventsourcing.commands.count",
		metric.WithDescription("Total number of commands handled, labelled by eventsourcing.result"),
		metric.WithUnit("{command}"),
	)

	CommandsDuration, _ = meter.Float64Histogram(
		"eventsourcing.commands.duration",
		metric.WithDescription("Command handling duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)

	CommandsProcessing, _ = meter.Int64UpDownCounter(
		"eventsourcing.command.processing",
		metric.WithDescription("Number of commands currently being processed"),
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

	// EventBus metrics
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

	EventBusDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventbus.duration",
		metric.WithDescription("Event bus handler duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// Query metrics
	QueriesCount, _ = meter.Int64Counter(
		"eventsourcing.queries.count",
		metric.WithDescription("Total number of queries handled, labelled by eventsourcing.result"),
		metric.WithUnit("{query}"),
	)

	QueriesDuration, _ = meter.Float64Histogram(
		"eventsourcing.queries.duration",
		metric.WithDescription("Query handling duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	QueriesProcessing, _ = meter.Int64UpDownCounter(
		"eventsourcing.query.processing",
		metric.WithDescription("Number of queries currently being processed"),
		metric.WithUnit("{query}"),
	)

	// EventStore metrics
	EventStoreSaves, _ = meter.Int64Counter(
		"eventsourcing.eventstore.saves",
		metric.WithDescription("Number of save operations"),
		metric.WithUnit("{operation}"),
	)

	EventStoreDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventstore.duration",
		metric.WithDescription("Event store operation duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
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
)
