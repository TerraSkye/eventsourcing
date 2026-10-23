package telemetry

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/terraskye/eventsourcing/projection"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	_ projection.Store           = TelemetryProjectionStore{}
	_ projection.Store           = telemetryDeadLetterStore{}
	_ projection.DeadLetterStore = telemetryDeadLetterStore{}
)

// TelemetryProjectionStore wraps a [projection.Store], tracing the units of
// work a [projection.Runner] commits through it. Create one with
// [WithProjectionStoreTelemetry].
//
// Commit runs inside a "process batch" span tagged with the projection's
// name, segment and new checkpoint position. The projection's handlers run
// within that span, so a handler wrapped with [WithEventTelemetry] shows up
// as its child, tagged with the projection's name and segment as well.
// Commit records [ProjectionBatchDuration]. Reset runs inside a "rebuild
// projection" span and records [ProjectionRebuilds]; Claim records
// [ProjectionClaims]. The other calls, made on every poll, are not traced.
type TelemetryProjectionStore struct {
	next projection.Store
	cfg  *config
}

// WithProjectionStoreTelemetry wraps next in a [TelemetryProjectionStore].
// Options such as [WithAttributes] add attributes to its spans; operation
// options are ignored.
//
// If next also implements [projection.DeadLetterStore], so does the
// returned store, and every skipped event is added as an event to the
// batch's span.
func WithProjectionStoreTelemetry(next projection.Store, options ...Option) projection.Store {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	store := TelemetryProjectionStore{next: next, cfg: cfg}
	if dls, ok := next.(projection.DeadLetterStore); ok {
		return telemetryDeadLetterStore{TelemetryProjectionStore: store, dls: dls}
	}
	return store
}

// leaseKey is the context key under which Commit records the batch's
// lease, for [WithEventTelemetry].
type leaseKey struct{}

// leaseAttrs returns the span attributes that identify l.
func leaseAttrs(l projection.Lease) []attribute.KeyValue {
	return []attribute.KeyValue{
		AttrProjectionName.String(l.Name),
		AttrProjectionSegment.Int(l.Segment),
	}
}

// spanAttrs returns the attributes for a span on l, including the
// configured ones.
func (t TelemetryProjectionStore) spanAttrs(ctx context.Context, l projection.Lease, extra ...attribute.KeyValue) []attribute.KeyValue {
	attrs := append(leaseAttrs(l), extra...)
	attrs = append(attrs, t.cfg.Attributes...)
	if t.cfg.GetAttributes != nil {
		attrs = append(attrs, t.cfg.GetAttributes(ctx)...)
	}
	return attrs
}

// Declare implements [projection.Store].
func (t TelemetryProjectionStore) Declare(ctx context.Context, name string, segments int, replayUntil uint64) error {
	return t.next.Declare(ctx, name, segments, replayUntil)
}

// Claim implements [projection.Store] and records [ProjectionClaims]. It is
// not traced: an instance on standby claims on every poll.
func (t TelemetryProjectionStore) Claim(ctx context.Context, l projection.Lease, ttl time.Duration) (projection.Lease, projection.Checkpoint, bool, error) {
	claimed, cp, ok, err := t.next.Claim(ctx, l, ttl)
	result := "acquired"
	switch {
	case err != nil:
		result = "error"
	case !ok:
		result = "held"
	}
	ProjectionClaims.Add(ctx, 1, metric.WithAttributes(AttrProjectionName.String(l.Name), AttrClaimResult.String(result)))
	return claimed, cp, ok, err
}

// Commit implements [projection.Store] inside a "process batch" span, and
// records [ProjectionBatchDuration].
func (t TelemetryProjectionStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, status projection.Status, fn func(ctx context.Context) error) error {
	startedAt := time.Now()
	ctx, span := tracer.Start(ctx, "process batch",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(t.spanAttrs(ctx, l,
			AttrProjectionPosition.Int64(int64(min(cp.Position, math.MaxInt64))),
			AttrProjectionPhase.String(status.Phase.String()),
		)...),
	)
	defer span.End()

	err := t.next.Commit(context.WithValue(ctx, leaseKey{}, l), l, cp, status, fn)

	result := "success"
	switch {
	case errors.Is(err, projection.ErrClaimLost):
		result = "claim_lost"
	case err != nil:
		result = "failure"
	}
	ProjectionBatchDuration.Record(ctx, time.Since(startedAt).Seconds(),
		metric.WithAttributes(AttrProjectionName.String(l.Name), AttrResult.String(result)))
	endSpan(span, err)
	return err
}

// Reset implements [projection.Store] inside a "rebuild projection" span,
// and records [ProjectionRebuilds].
func (t TelemetryProjectionStore) Reset(ctx context.Context, l projection.Lease, newVersion int, wipe func(ctx context.Context) error) error {
	ctx, span := tracer.Start(ctx, "rebuild projection",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(t.spanAttrs(ctx, l)...),
	)
	defer span.End()

	err := t.next.Reset(ctx, l, newVersion, wipe)

	result := "success"
	if err != nil {
		result = "failure"
	}
	ProjectionRebuilds.Add(ctx, 1, metric.WithAttributes(AttrProjectionName.String(l.Name), AttrResult.String(result)))
	endSpan(span, err)
	return err
}

// Release implements [projection.Store].
func (t TelemetryProjectionStore) Release(ctx context.Context, l projection.Lease) error {
	return t.next.Release(ctx, l)
}

// SaveStatus implements [projection.Store].
func (t TelemetryProjectionStore) SaveStatus(ctx context.Context, l projection.Lease, status projection.Status) error {
	return t.next.SaveStatus(ctx, l, status)
}

// LoadStatuses implements [projection.Store].
func (t TelemetryProjectionStore) LoadStatuses(ctx context.Context, name string) ([]projection.Status, error) {
	return t.next.LoadStatuses(ctx, name)
}

// RequestRebuild implements [projection.Store].
func (t TelemetryProjectionStore) RequestRebuild(ctx context.Context, name string) (uint64, error) {
	return t.next.RequestRebuild(ctx, name)
}

// Rebuilds implements [projection.Store].
func (t TelemetryProjectionStore) Rebuilds(ctx context.Context, name string) (requested, done uint64, err error) {
	return t.next.Rebuilds(ctx, name)
}

// Heartbeat implements [projection.Store].
func (t TelemetryProjectionStore) Heartbeat(ctx context.Context, name, owner string, version int, ttl time.Duration) ([]projection.Member, error) {
	return t.next.Heartbeat(ctx, name, owner, version, ttl)
}

// telemetryDeadLetterStore is a [TelemetryProjectionStore] around a store
// that also implements [projection.DeadLetterStore].
type telemetryDeadLetterStore struct {
	TelemetryProjectionStore
	dls projection.DeadLetterStore
}

// SaveDeadLetter implements [projection.DeadLetterStore], adding a
// "dead letter" event to the batch's span.
func (t telemetryDeadLetterStore) SaveDeadLetter(ctx context.Context, l projection.Lease, d projection.DeadLetter) error {
	trace.SpanFromContext(ctx).AddEvent("dead letter", trace.WithAttributes(
		AttrEventGlobalPos.Int64(int64(min(d.Position, math.MaxInt64))),
		AttrStreamID.String(d.StreamID),
		AttrEventType.String(d.EventType),
		AttrErrorMessage.String(d.Error),
	))
	return t.dls.SaveDeadLetter(ctx, l, d)
}

// LoadDeadLetters implements [projection.DeadLetterStore].
func (t telemetryDeadLetterStore) LoadDeadLetters(ctx context.Context, name string, segment int) ([]projection.DeadLetter, error) {
	return t.dls.LoadDeadLetters(ctx, name, segment)
}

// DeleteDeadLetter implements [projection.DeadLetterStore].
func (t telemetryDeadLetterStore) DeleteDeadLetter(ctx context.Context, l projection.Lease, position uint64) error {
	return t.dls.DeleteDeadLetter(ctx, l, position)
}

// endSpan sets span's status from err.
func endSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetStatus(codes.Ok, "")
}
