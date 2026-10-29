package telemetry

import (
	"context"
	"math"

	"github.com/terraskye/eventsourcing/projection"
	"go.opentelemetry.io/otel/metric"
)

// phases lists every [projection.Phase], for the phase gauge.
var phases = []projection.Phase{
	projection.Starting,
	projection.Standby,
	projection.CatchingUp,
	projection.Live,
	projection.Stalled,
}

// Observable projection instruments, reported by [ObserveProjection].
var (
	// ProjectionLag reports how far a projection lags behind the head of
	// the global log: its Head minus its Position, in GlobalPosition units.
	// On stores that number events densely (memory, file, Postgres) that is
	// a count of events; on KurrentDB GlobalPosition is a commit position,
	// so the lag is in bytes of log and only its trend is meaningful.
	ProjectionLag, _ = meter.Int64ObservableGauge(
		"eventsourcing.projection.lag",
		metric.WithDescription("Head of the global log minus the projection's checkpoint, in global positions"),
		metric.WithUnit("{position}"),
	)

	// ProjectionPosition reports a projection's checkpoint: the
	// GlobalPosition up to which it has processed the log.
	ProjectionPosition, _ = meter.Int64ObservableGauge(
		"eventsourcing.projection.position",
		metric.WithDescription("Global position up to which the projection has processed the log"),
		metric.WithUnit("{position}"),
	)

	// ProjectionPhase reports a projection's [projection.Phase]: one series
	// per phase, labelled by [AttrProjectionPhase], that is 1 for the
	// current phase and 0 for the others.
	ProjectionPhase, _ = meter.Int64ObservableGauge(
		"eventsourcing.projection.phase",
		metric.WithDescription("1 for the projection's current phase, 0 for the others, labelled by eventsourcing.projection.phase"),
		metric.WithUnit("1"),
	)

	// ProjectionDeadLettered counts events a projection dead-lettered: its
	// handler failed on them under a non-stalling error policy, and the
	// runner moved past them. It reports [projection.Runner.Skipped], which
	// counts the same events under the policies' name, "skip".
	ProjectionDeadLettered, _ = meter.Int64ObservableCounter(
		"eventsourcing.projection.dead_lettered.events",
		metric.WithDescription("Events a projection dead-lettered because its handler failed"),
		metric.WithUnit("{event}"),
	)
)

// ObserveProjection reports r's status as metrics, labelled by
// [AttrProjectionName]: [ProjectionLag], [ProjectionPosition],
// [ProjectionPhase] and [ProjectionDeadLettered]. The values come from
// [projection.Runner.Status], as seen by this instance, and are read each
// time the metrics are collected.
//
// Call Unregister on the returned registration when r is no longer run.
func ObserveProjection(r *projection.Runner) (metric.Registration, error) {
	name := AttrProjectionName.String(r.Name())
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := r.Status()
		o.ObserveInt64(ProjectionLag, toInt64(st.Head-min(st.Position, st.Head)), metric.WithAttributes(name))
		o.ObserveInt64(ProjectionPosition, toInt64(st.Position), metric.WithAttributes(name))
		for _, p := range phases {
			var current int64
			if p == st.Phase {
				current = 1
			}
			o.ObserveInt64(ProjectionPhase, current, metric.WithAttributes(name, AttrProjectionPhase.String(p.String())))
		}
		o.ObserveInt64(ProjectionDeadLettered, r.Skipped(), metric.WithAttributes(name))
		return nil
	}, ProjectionLag, ProjectionPosition, ProjectionPhase, ProjectionDeadLettered)
}

// toInt64 converts a GlobalPosition to an int64 metric value, saturating
// instead of wrapping.
func toInt64(v uint64) int64 {
	return int64(min(v, math.MaxInt64))
}
