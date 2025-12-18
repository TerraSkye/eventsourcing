package eventsourcing

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
)

const (
	instrumentationName = "github.com/terraskye/eventsourcing"

	AttrCommandType    = "eventsourcing.command.type"
	AttrAggregateID    = "eventsourcing.aggregate.id"
	AttrStreamID       = "eventsourcing.stream.id"
	AttrEventType      = "eventsourcing.event.type"
	AttrEventCount     = "eventsourcing.events.count"
	AttrStreamVersion  = "eventsourcing.stream.version"
	AttrQueryType      = "eventsourcing.query.type"
	AttrErrorType      = "eventsourcing.error.type"
	AttrSubscriberName = "eventsourcing.subscriber.name"
)

var (
	meter  = otel.Meter(instrumentationName)
	tracer = otel.Tracer(instrumentationName)

	// Command metrics - these are always safe to use
	CommandsHandled, _ = meter.Int64Counter(
		"eventsourcing.commands.handled",
		metric.WithDescription("Number of commands handled"),
		metric.WithUnit("{command}"),
	)

	CommandsDuration, _ = meter.Float64Histogram(
		"eventsourcing.commands.duration",
		metric.WithDescription("Command handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000),
	)

	CommandsInFlight, _ = meter.Int64UpDownCounter(
		"eventsourcing.commands.in_flight",
		metric.WithDescription("Number of commands currently being processed"),
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

	// EventBus metrics
	EventBusPublished, _ = meter.Int64Counter(
		"eventsourcing.eventbus.published",
		metric.WithDescription("Number of events published to event bus"),
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

	// Query metrics
	QueriesHandled, _ = meter.Int64Counter(
		"eventsourcing.queries.handled",
		metric.WithDescription("Number of queries handled"),
		metric.WithUnit("{query}"),
	)

	QueriesDuration, _ = meter.Float64Histogram(
		"eventsourcing.queries.duration",
		metric.WithDescription("Query handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000),
	)

	// System metrics
	ConcurrencyConflicts, _ = meter.Int64Counter(
		"eventsourcing.concurrency.conflicts",
		metric.WithDescription("Number of concurrency conflicts"),
		metric.WithUnit("{conflict}"),
	)

	StreamVersions, _ = meter.Int64Gauge(
		"eventsourcing.stream.version",
		metric.WithDescription("Current version of streams"),
		metric.WithUnit("{version}"),
	)
)
