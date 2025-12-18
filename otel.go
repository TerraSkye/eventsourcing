package eventsourcing

import (
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	instrumentationName = "github.com/terraskye/eventsourcing"
)

var (
	// Meters
	meter  metric.Meter
	tracer trace.Tracer

	// Command metrics
	CommandsHandled  metric.Int64Counter
	CommandsDuration metric.Float64Histogram
	CommandsInFlight metric.Int64UpDownCounter

	// Event metrics
	EventsAppended metric.Int64Counter
	EventsLoaded   metric.Int64Counter

	// EventBus metrics
	EventBusPublished   metric.Int64Counter
	EventBusErrors      metric.Int64Counter
	EventBusSubscribers metric.Int64UpDownCounter

	// Query metrics
	QueriesHandled  metric.Int64Counter
	QueriesDuration metric.Float64Histogram

	// System metrics
	ConcurrencyConflicts metric.Int64Counter
	StreamVersions       metric.Int64Gauge

	// Initialization
	once        sync.Once
	initErr     error
	initialized bool
)

// Init initializes the global metrics
// Call this once at application startup
func Init() error {
	once.Do(func() {
		meter = otel.Meter(instrumentationName)
		initErr = initializeMetrics()
		if initErr == nil {
			initialized = true
		}
	})
	return initErr
}

func initializeMetrics() error {
	var err error

	// Command metrics
	CommandsHandled, err = meter.Int64Counter(
		"eventsourcing.commands.handled",
		metric.WithDescription("Number of commands handled"),
		metric.WithUnit("{command}"),
	)
	if err != nil {
		return err
	}

	CommandsDuration, err = meter.Float64Histogram(
		"eventsourcing.commands.duration",
		metric.WithDescription("Command handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000),
	)
	if err != nil {
		return err
	}

	CommandsInFlight, err = meter.Int64UpDownCounter(
		"eventsourcing.commands.in_flight",
		metric.WithDescription("Number of commands currently being processed"),
		metric.WithUnit("{command}"),
	)
	if err != nil {
		return err
	}

	// Event metrics
	EventsAppended, err = meter.Int64Counter(
		"eventsourcing.events.appended",
		metric.WithDescription("Number of events appended to streams"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return err
	}

	EventsLoaded, err = meter.Int64Counter(
		"eventsourcing.events.loaded",
		metric.WithDescription("Number of events loaded from streams"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return err
	}

	// EventBus metrics
	EventBusPublished, err = meter.Int64Counter(
		"eventsourcing.eventbus.published",
		metric.WithDescription("Number of events published to event bus"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		return err
	}

	EventBusErrors, err = meter.Int64Counter(
		"eventsourcing.eventbus.errors",
		metric.WithDescription("Number of event bus handler errors"),
		metric.WithUnit("{error}"),
	)
	if err != nil {
		return err
	}

	EventBusSubscribers, err = meter.Int64UpDownCounter(
		"eventsourcing.eventbus.subscribers",
		metric.WithDescription("Number of active event bus subscribers"),
		metric.WithUnit("{subscriber}"),
	)
	if err != nil {
		return err
	}

	// Query metrics
	QueriesHandled, err = meter.Int64Counter(
		"eventsourcing.queries.handled",
		metric.WithDescription("Number of queries handled"),
		metric.WithUnit("{query}"),
	)
	if err != nil {
		return err
	}

	QueriesDuration, err = meter.Float64Histogram(
		"eventsourcing.queries.duration",
		metric.WithDescription("Query handling duration"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(1, 5, 10, 25, 50, 100, 250, 500, 1000),
	)
	if err != nil {
		return err
	}

	// System metrics
	ConcurrencyConflicts, err = meter.Int64Counter(
		"eventsourcing.concurrency.conflicts",
		metric.WithDescription("Number of concurrency conflicts"),
		metric.WithUnit("{conflict}"),
	)
	if err != nil {
		return err
	}

	StreamVersions, err = meter.Int64Gauge(
		"eventsourcing.stream.version",
		metric.WithDescription("Current version of streams"),
		metric.WithUnit("{version}"),
	)
	if err != nil {
		return err
	}

	return nil
}

// IsInitialized returns whether metrics have been initialized
func IsInitialized() bool {
	return initialized
}

// MustInit initializes metrics and panics on error
// Use this in main() for fail-fast behavior
func MustInit() {
	if err := Init(); err != nil {
		panic("failed to initialize metrics: " + err.Error())
	}
}
