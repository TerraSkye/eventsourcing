package telemetry

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
	// AttrDBSystemName identifies the database system behind an event
	// store, following the OpenTelemetry semantic conventions, such as
	// "postgresql" or "sqlite". Set it with [WithAttributes] on
	// [WithEventStoreTelemetry]; the store's spans and metrics then carry
	// it. Unset, they carry none.
	AttrDBSystemName = attribute.Key("db.system.name")

	// Command attributes
	AttrCommandType = attribute.Key("eventsourcing.command.type")
	AttrAggregateID = attribute.Key("eventsourcing.aggregate.id")

	// Stream attributes
	AttrStreamID      = attribute.Key("eventsourcing.stream.id")
	AttrStreamVersion = attribute.Key("eventsourcing.stream.version")

	// EventData attributes
	AttrEventType      = attribute.Key("eventsourcing.event.type")
	AttrEventID        = attribute.Key("eventsourcing.event.id")
	AttrEventCount     = attribute.Key("eventsourcing.event.count")
	AttrEventGlobalPos = attribute.Key("eventsourcing.event.global_position")
	AttrEventStreamPos = attribute.Key("eventsourcing.event.stream_position")

	// Chain attributes: see eventsourcing.Envelope.CorrelationID and
	// CausationID.
	AttrCorrelationID = attribute.Key("eventsourcing.correlation.id")
	AttrCausationID   = attribute.Key("eventsourcing.causation.id")

	// Query attributes
	AttrQueryType = attribute.Key("eventsourcing.query.type")
	AttrQueryID   = attribute.Key("eventsourcing.query.id")

	// AttrResult labels an outcome: "success" or "failure", and also
	// "rejected" for a command, "skipped" for an event handler and
	// "claim_lost" for a projection batch.
	AttrResult = attribute.Key("eventsourcing.result")

	// AttrErrorMessage carries the cause of a business rule violation on a
	// command span.
	AttrErrorMessage = attribute.Key("eventsourcing.error.message")

	// Projection attributes
	AttrProjectionName     = attribute.Key("eventsourcing.projection.name")
	AttrProjectionSegment  = attribute.Key("eventsourcing.projection.segment")
	AttrProjectionPosition = attribute.Key("eventsourcing.projection.position")
	AttrProjectionPhase    = attribute.Key("eventsourcing.projection.phase")
	AttrClaimResult        = attribute.Key("eventsourcing.projection.claim.result") // "acquired" | "held" | "error"

	// AttrOperation names the event store operation: "save", "load" or
	// "read_all".
	AttrOperation = attribute.Key("eventsourcing.operation")
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

	// CommandDuration records command handling duration, in seconds,
	// labelled by [AttrCommandType] and [AttrResult]: "success",
	// "rejected" for a [eventsourcing.BusinessRuleViolationError], or
	// "failure". Its count is the number of commands handled.
	CommandDuration, _ = meter.Float64Histogram(
		"eventsourcing.command.duration",
		metric.WithDescription("Command handling duration, labelled by eventsourcing.result"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)

	// CommandActive tracks the number of commands being handled.
	CommandActive, _ = meter.Int64UpDownCounter(
		"eventsourcing.command.active",
		metric.WithDescription("Number of commands being handled"),
		metric.WithUnit("{command}"),
	)

	// CommandConflicts counts commands that failed with a
	// [eventsourcing.StreamRevisionConflictError].
	CommandConflicts, _ = meter.Int64Counter(
		"eventsourcing.command.conflicts",
		metric.WithDescription("Number of commands that failed with a concurrency conflict"),
		metric.WithUnit("{conflict}"),
	)

	// Query metrics

	// QueryDuration records query handling duration, in seconds, labelled
	// by [AttrQueryType] and [AttrResult] ("success" or "failure"). Its
	// count is the number of queries handled.
	QueryDuration, _ = meter.Float64Histogram(
		"eventsourcing.query.duration",
		metric.WithDescription("Query handling duration, labelled by eventsourcing.result"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// QueryActive tracks the number of queries being handled.
	QueryActive, _ = meter.Int64UpDownCounter(
		"eventsourcing.query.active",
		metric.WithDescription("Number of queries being handled"),
		metric.WithUnit("{query}"),
	)

	// Event handler metrics

	// EventHandlerDuration records how long an event handler took, in
	// seconds, labelled by [AttrEventType] and [AttrResult]: "success",
	// "skipped" for a [eventsourcing.SkippedEventError], or "failure". Its
	// count is the number of events handled.
	EventHandlerDuration, _ = meter.Float64Histogram(
		"eventsourcing.event_handler.duration",
		metric.WithDescription("Event handler duration, labelled by eventsourcing.result"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// Event store metrics

	// EventStoreDuration records event store operation duration, in
	// seconds, labelled by [AttrOperation], [AttrResult] and, when set,
	// [AttrDBSystemName]. Its count is the number of operations.
	EventStoreDuration, _ = meter.Float64Histogram(
		"eventsourcing.event_store.duration",
		metric.WithDescription("Event store operation duration, labelled by eventsourcing.operation and eventsourcing.result"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
	)

	// EventStoreAppended counts events an event store appended. It carries
	// no stream ID, since one series per stream would grow without bound;
	// the store spans carry it instead.
	EventStoreAppended, _ = meter.Int64Counter(
		"eventsourcing.event_store.appended.events",
		metric.WithDescription("Number of events appended"),
		metric.WithUnit("{event}"),
	)

	// EventStoreLoaded counts events an event store loaded, from streams
	// or the global log, with no stream ID for the same reason as
	// [EventStoreAppended].
	EventStoreLoaded, _ = meter.Int64Counter(
		"eventsourcing.event_store.loaded.events",
		metric.WithDescription("Number of events loaded"),
		metric.WithUnit("{event}"),
	)

	// Projection metrics

	// ProjectionBatchDuration records how long a projection batch took to
	// commit, handlers included, in seconds, labelled by
	// [AttrProjectionName] and [AttrResult] ("success", "failure" or
	// "claim_lost"). Its count is the number of batches.
	ProjectionBatchDuration, _ = meter.Float64Histogram(
		"eventsourcing.projection.batch.duration",
		metric.WithDescription("Duration of projection batch commits, handlers included"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)

	// ProjectionClaims counts attempts to claim a projection segment,
	// labelled by [AttrProjectionName] and [AttrClaimResult].
	ProjectionClaims, _ = meter.Int64Counter(
		"eventsourcing.projection.claims",
		metric.WithDescription("Attempts to claim a projection segment, labelled by eventsourcing.projection.claim.result"),
		metric.WithUnit("{claim}"),
	)

	// ProjectionRebuilds counts projection segment rebuilds, labelled by
	// [AttrProjectionName] and [AttrResult].
	ProjectionRebuilds, _ = meter.Int64Counter(
		"eventsourcing.projection.rebuilds",
		metric.WithDescription("Projection segment rebuilds, labelled by eventsourcing.result"),
		metric.WithUnit("{rebuild}"),
	)

	// ProjectionHeartbeats counts the heartbeats a runner sends to record
	// itself as a live instance of a projection, labelled by
	// [AttrProjectionName] and [AttrResult] ("success" or "failure"). While
	// they fail, the instance claims no new segments.
	ProjectionHeartbeats, _ = meter.Int64Counter(
		"eventsourcing.projection.heartbeats",
		metric.WithDescription("Heartbeats a projection instance sends, labelled by eventsourcing.result"),
		metric.WithUnit("{heartbeat}"),
	)

	// ProjectionInstanceCount records the number of live instances of a
	// projection, as the last heartbeat listed them, labelled by
	// [AttrProjectionName]. Every instance reports the same count, so
	// aggregate it with max rather than sum.
	ProjectionInstanceCount, _ = meter.Int64Gauge(
		"eventsourcing.projection.instance.count",
		metric.WithDescription("Live instances of a projection, as its last heartbeat listed them"),
		metric.WithUnit("{instance}"),
	)

	// ProjectionSegmentHeld records the number of segments of a projection
	// this instance holds, as of its last heartbeat, labelled by
	// [AttrProjectionName]. Summed over the instances it is the segment
	// count once every segment is claimed.
	ProjectionSegmentHeld, _ = meter.Int64Gauge(
		"eventsourcing.projection.segment.held",
		metric.WithDescription("Segments of a projection this instance holds, as of its last heartbeat"),
		metric.WithUnit("{segment}"),
	)
)
