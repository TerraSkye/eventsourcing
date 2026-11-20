package telemetry

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/terraskye/eventsourcing/automation"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// automationPhases lists every [automation.Phase], for the phase gauge.
var automationPhases = []automation.Phase{
	automation.Starting,
	automation.Waiting,
	automation.Idle,
	automation.Working,
	automation.Stalled,
	automation.Paused,
}

// Observable automation instruments, reported by [ObserveAutomation].
var (
	// AutomationPhase reports an automation's [automation.Phase] on this
	// instance: one series per phase, labelled by [AttrAutomationPhase],
	// that is 1 for the current phase and 0 for the others.
	AutomationPhase, _ = meter.Int64ObservableGauge(
		"eventsourcing.automation.phase",
		metric.WithDescription("1 for the automation's current phase, 0 for the others, labelled by eventsourcing.automation.phase"),
		metric.WithUnit("1"),
	)

	// AutomationItems counts the items an automation worked on this
	// instance since it started, labelled by [AttrResult]: "success",
	// "failure" or "deferred". It reports [automation.Status.Processed],
	// Failed and Deferred.
	AutomationItems, _ = meter.Int64ObservableCounter(
		"eventsourcing.automation.items",
		metric.WithDescription("Items an automation worked, labelled by eventsourcing.result"),
		metric.WithUnit("{item}"),
	)
)

// AutomationStatusReporter is what [ObserveAutomation] reads: an
// *automation.Runner of any item type.
type AutomationStatusReporter interface {
	Name() string
	Status() automation.Status
}

// ObserveAutomation reports r's status as metrics, labelled by
// [AttrAutomationName]: [AutomationPhase] and [AutomationItems]. The values
// come from [automation.Runner.Status], as seen by this instance, and are
// read each time the metrics are collected.
//
// Call Unregister on the returned registration when r is no longer run.
func ObserveAutomation(r AutomationStatusReporter) (metric.Registration, error) {
	name := AttrAutomationName.String(r.Name())
	return meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := r.Status()
		for _, p := range automationPhases {
			var current int64
			if p == st.Phase {
				current = 1
			}
			o.ObserveInt64(AutomationPhase, current, metric.WithAttributes(name, AttrAutomationPhase.String(p.String())))
		}
		for result, n := range map[string]uint64{"success": st.Processed, "failure": st.Failed, "deferred": st.Deferred} {
			o.ObserveInt64(AutomationItems, int64(min(n, math.MaxInt64)), metric.WithAttributes(name, AttrResult.String(result)))
		}
		return nil
	}, AutomationPhase, AutomationItems)
}

// WithWorkTelemetry wraps the work of the automation name with
// OpenTelemetry tracing and metrics, returning an [automation.WorkFunc] to
// put in [automation.Definition.Work]:
//
//	ship := shipping.NewShipOrders(list, todoRunner, commandBus).Definition()
//	ship.Work = telemetry.WithWorkTelemetry(ship.Name, ship.Work)
//	ship.Store = postgres.NewAutomationStore(pool)
//	go automation.NewRunner(ship).Run(ctx)
//
// Each item runs inside a "process item" span tagged with the
// automation's name, so a command the work dispatches through a bus with
// [CommandTelemetry] shows up as its child. It records
// [AutomationWorkDuration] for every item, labelled by [AttrResult]. A
// returned automation.Deferred is not an error: it leaves the span's
// status unset, as a success does, adds a "segment_deferred" event to
// it, with the time the segment is paused until as
// eventsourcing.automation.deferred_until when the deferral names one,
// and is recorded with result "deferred". Work that returns the
// cancellation of its context, because the run is stopping, is recorded
// with result "canceled" and also leaves the span's status unset, as the
// runner does not count it either. Any other error marks the span as
// failed and is recorded with result "failure" and an [AttrErrorType];
// so is a lease deadline the work ran past, which the runner counts as a
// failure.
//
// [WithOperation] and [WithOperationGetter] rename the span, and
// [WithAttributes] and [WithAttributeGetter] add attributes to it.
func WithWorkTelemetry[T any](name string, work automation.WorkFunc[T], options ...Option) automation.WorkFunc[T] {
	cfg := &config{}
	for _, o := range options {
		o.apply(cfg)
	}
	nameAttr := AttrAutomationName.String(name)
	return func(ctx context.Context, item T) error {
		attrs := append([]attribute.KeyValue{nameAttr}, cfg.Attributes...)
		if cfg.GetAttributes != nil {
			attrs = append(attrs, cfg.GetAttributes(ctx)...)
		}

		operation := "process item"
		if cfg.Operation != "" {
			operation = cfg.Operation
		}
		if cfg.GetOperation != nil {
			if op := cfg.GetOperation(ctx, operation); op != "" {
				operation = op
			}
		}

		ctx, span := tracer.Start(ctx, operation,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(attrs...),
		)
		defer span.End()

		startedAt := time.Now()
		err := work(ctx, item)
		duration := time.Since(startedAt).Seconds()

		metricAttrs := []attribute.KeyValue{nameAttr}
		var deferred *automation.DeferredError
		switch {
		case err == nil:
			metricAttrs = append(metricAttrs, AttrResult.String("success"))
		case errors.As(err, &deferred):
			metricAttrs = append(metricAttrs, AttrResult.String("deferred"))
			var evAttrs []attribute.KeyValue
			if !deferred.Until.IsZero() {
				evAttrs = append(evAttrs, attribute.String("eventsourcing.automation.deferred_until", deferred.Until.Format(time.RFC3339)))
			}
			span.AddEvent("segment_deferred", trace.WithAttributes(evAttrs...))
		case ctx.Err() == context.Canceled && errors.Is(err, context.Canceled):
			// The run is stopping: the runner does not count the item, so
			// it is neither a success nor a failure here.
			metricAttrs = append(metricAttrs, AttrResult.String("canceled"))
		default:
			errType := AttrErrorType.String(errorType(err))
			metricAttrs = append(metricAttrs, AttrResult.String("failure"), errType)
			span.SetAttributes(errType)
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		AutomationWorkDuration.Record(ctx, duration, metric.WithAttributes(metricAttrs...))
		return err
	}
}
