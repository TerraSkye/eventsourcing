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

// Semantic attribute keys used on the spans and metrics produced by this
// package, following OpenTelemetry naming conventions.
const (
	// AttrDBSystem identifies the database system, following the OpenTelemetry
	// semantic conventions.
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

// meter and tracer are the shared OpenTelemetry instruments used by every
// telemetry wrapper in this package; all spans and metrics are reported
// under instrumentationName.
var (
	meter  = otel.Meter(instrumentationName, metric.WithInstrumentationVersion(eventsourcing.InstrumentationVersion))
	tracer = otel.Tracer(instrumentationName, trace.WithInstrumentationVersion(eventsourcing.InstrumentationVersion))
)

// Metric instruments recorded by the telemetry wrappers in this package.
// They are exported so callers can read their descriptors (name, unit,
// description) when wiring up custom views or exporters; they are not meant
// to be recorded to directly.
var (
	// Command metrics

	// CommandsCount counts commands handled, labelled by
	// [AttrResult] ("success" or "failure").
	CommandsCount, _ = meter.Int64Counter(
		"eventsourcing.commands.count",
		metric.WithDescription("Total number of commands handled, labelled by eventsourcing.result"),
		metric.WithUnit("{command}"),
	)

	// CommandsDuration records command handling duration, in seconds.
	CommandsDuration, _ = meter.Float64Histogram(
		"eventsourcing.commands.duration",
		metric.WithDescription("Command handling duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)

	// CommandsProcessing tracks the number of commands currently being
	// processed.
	CommandsProcessing, _ = meter.Int64UpDownCounter(
		"eventsourcing.command.processing",
		metric.WithDescription("Number of commands currently being processed"),
		metric.WithUnit("{command}"),
	)

	// EventData metrics

	// EventsAppended counts events appended to streams.
	EventsAppended, _ = meter.Int64Counter(
		"eventsourcing.events.appended",
		metric.WithDescription("Number of events appended to streams"),
		metric.WithUnit("{event}"),
	)

	// EventsLoaded counts events loaded from streams.
	EventsLoaded, _ = meter.Int64Counter(
		"eventsourcing.events.loaded",
		metric.WithDescription("Number of events loaded from streams"),
		metric.WithUnit("{event}"),
	)

	// EventBus metrics

	// EventBusHandled counts events handled by subscribers.
	EventBusHandled, _ = meter.Int64Counter(
		"eventsourcing.eventbus.handled",
		metric.WithDescription("Number of events handled by subscribers"),
		metric.WithUnit("{event}"),
	)

	// EventBusErrors counts event bus handler errors.
	EventBusErrors, _ = meter.Int64Counter(
		"eventsourcing.eventbus.errors",
		metric.WithDescription("Number of event bus handler errors"),
		metric.WithUnit("{error}"),
	)

	// EventBusDuration records event bus handler duration, in seconds.
	EventBusDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventbus.duration",
		metric.WithDescription("Event bus handler duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// Query metrics

	// QueriesCount counts queries handled, labelled by [AttrResult]
	// ("success" or "failure").
	QueriesCount, _ = meter.Int64Counter(
		"eventsourcing.queries.count",
		metric.WithDescription("Total number of queries handled, labelled by eventsourcing.result"),
		metric.WithUnit("{query}"),
	)

	// QueriesDuration records query handling duration, in seconds.
	QueriesDuration, _ = meter.Float64Histogram(
		"eventsourcing.queries.duration",
		metric.WithDescription("Query handling duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// QueriesProcessing tracks the number of queries currently being
	// processed.
	QueriesProcessing, _ = meter.Int64UpDownCounter(
		"eventsourcing.query.processing",
		metric.WithDescription("Number of queries currently being processed"),
		metric.WithUnit("{query}"),
	)

	// EventStore metrics

	// EventStoreSaves counts EventStore save operations.
	EventStoreSaves, _ = meter.Int64Counter(
		"eventsourcing.eventstore.saves",
		metric.WithDescription("Number of save operations"),
		metric.WithUnit("{operation}"),
	)

	// EventStoreDuration records EventStore operation duration, in seconds.
	EventStoreDuration, _ = meter.Float64Histogram(
		"eventsourcing.eventstore.duration",
		metric.WithDescription("Event store operation duration"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// EventStoreErrors counts EventStore errors.
	EventStoreErrors, _ = meter.Int64Counter(
		"eventsourcing.eventstore.errors",
		metric.WithDescription("Number of event store errors"),
		metric.WithUnit("{error}"),
	)

	// System metrics

	// ConcurrencyConflicts counts detected
	// [eventsourcing.StreamRevisionConflictError] occurrences.
	ConcurrencyConflicts, _ = meter.Int64Counter(
		"eventsourcing.concurrency.conflicts",
		metric.WithDescription("Number of concurrency conflicts"),
		metric.WithUnit("{conflict}"),
	)
)
