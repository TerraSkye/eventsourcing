package telemetry

import (
	"context"
	"slices"
	"time"

	"github.com/terraskye/eventsourcing/automation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var _ automation.Store = AutomationStore{}

// AutomationStore wraps an [automation.Store], recording how an
// automation's instances share its segments. Create one with
// [WithAutomationStoreTelemetry].
//
// Acquire records [AutomationAcquisitions], [AutomationSegmentGrants] and
// [AutomationSegmentWaiting]; Renew and Release record
// [AutomationSegmentLost]. They run on every poll, so they are not traced.
// A deferral (Defer, or a Release with DeferFor) adds a "segment deferred"
// event to the current span, which is the work's span when the work is
// wrapped with [WithWorkTelemetry]. SetPaused, an operator's action, runs
// inside a "pause automation" or "resume automation" span.
type AutomationStore struct {
	next automation.Store
	cfg  *config
}

// WithAutomationStoreTelemetry wraps next in an [AutomationStore]. Options
// such as [WithAttributes] add attributes to its spans; operation options
// are ignored.
func WithAutomationStoreTelemetry(next automation.Store, options ...Option) automation.Store {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	return AutomationStore{next: next, cfg: cfg}
}

// Acquire implements [automation.Store].
func (t AutomationStore) Acquire(ctx context.Context, name, owner string, candidates []automation.Segment, max int, ttl time.Duration) (automation.AcquireResult, error) {
	res, err := t.next.Acquire(ctx, name, owner, candidates, max, ttl)
	nameAttr := AttrAutomationName.String(name)
	if err != nil {
		AutomationAcquisitions.Add(ctx, 1, metric.WithAttributes(nameAttr, AttrResult.String("failure")))
		return res, err
	}
	AutomationAcquisitions.Add(ctx, 1, metric.WithAttributes(nameAttr, AttrResult.String("success")))
	if n := len(res.Granted); n > 0 {
		AutomationSegmentGrants.Add(ctx, int64(n), metric.WithAttributes(nameAttr))
	}
	AutomationSegmentWaiting.Record(ctx, int64(res.Eligible), metric.WithAttributes(nameAttr))
	return res, nil
}

// Renew implements [automation.Store].
func (t AutomationStore) Renew(ctx context.Context, held []automation.SegmentLease, ttl time.Duration) ([]automation.SegmentLease, error) {
	lost, err := t.next.Renew(ctx, held, ttl)
	countLost(ctx, lost)
	return lost, err
}

// Release implements [automation.Store].
func (t AutomationStore) Release(ctx context.Context, rel []automation.ReleaseRequest) ([]automation.SegmentLease, error) {
	lost, err := t.next.Release(ctx, rel)
	if err == nil {
		span := trace.SpanFromContext(ctx)
		for _, rr := range rel {
			if rr.DeferFor > 0 && !slices.Contains(lost, rr.Lease) {
				span.AddEvent("segment deferred", trace.WithAttributes(
					AttrAutomationName.String(rr.Lease.Name),
					AttrAutomationSegment.String(rr.Lease.Key),
					attribute.Float64("eventsourcing.automation.deferred_for", rr.DeferFor.Seconds()),
				))
			}
		}
	}
	countLost(ctx, lost)
	return lost, err
}

// Defer implements [automation.Store].
func (t AutomationStore) Defer(ctx context.Context, name, key string, d time.Duration) error {
	err := t.next.Defer(ctx, name, key, d)
	if err == nil {
		trace.SpanFromContext(ctx).AddEvent("segment deferred", trace.WithAttributes(
			AttrAutomationName.String(name),
			AttrAutomationSegment.String(key),
			attribute.Float64("eventsourcing.automation.deferred_for", d.Seconds()),
		))
	}
	return err
}

// DeferredFor implements [automation.Store].
func (t AutomationStore) DeferredFor(ctx context.Context, name, key string) (time.Duration, error) {
	return t.next.DeferredFor(ctx, name, key)
}

// SetPaused implements [automation.Store] inside a "pause automation" or
// "resume automation" span.
func (t AutomationStore) SetPaused(ctx context.Context, name string, paused bool) error {
	operation := "resume automation"
	if paused {
		operation = "pause automation"
	}
	attrs := append([]attribute.KeyValue{AttrAutomationName.String(name)}, t.cfg.Attributes...)
	if t.cfg.GetAttributes != nil {
		attrs = append(attrs, t.cfg.GetAttributes(ctx)...)
	}
	ctx, span := tracer.Start(ctx, operation, trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	defer span.End()
	err := t.next.SetPaused(ctx, name, paused)
	endSpan(span, err)
	return err
}

// Paused implements [automation.Store].
func (t AutomationStore) Paused(ctx context.Context, name string) (bool, error) {
	return t.next.Paused(ctx, name)
}

// SaveStatus implements [automation.Store].
func (t AutomationStore) SaveStatus(ctx context.Context, name string, status automation.Status, ttl time.Duration) error {
	return t.next.SaveStatus(ctx, name, status, ttl)
}

// LoadStatuses implements [automation.Store].
func (t AutomationStore) LoadStatuses(ctx context.Context, name string) ([]automation.Status, error) {
	return t.next.LoadStatuses(ctx, name)
}

// LoadSegments implements [automation.Store].
func (t AutomationStore) LoadSegments(ctx context.Context, name string) ([]automation.SegmentState, error) {
	return t.next.LoadSegments(ctx, name)
}

// countLost records [AutomationSegmentLost] for every lost lease.
func countLost(ctx context.Context, lost []automation.SegmentLease) {
	for _, l := range lost {
		AutomationSegmentLost.Add(ctx, 1, metric.WithAttributes(AttrAutomationName.String(l.Name)))
	}
}
