package telemetry

import (
	"context"
	"os"
	"sync"
	"testing"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricembedded "go.opentelemetry.io/otel/metric/embedded"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	traceembedded "go.opentelemetry.io/otel/trace/embedded"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// durationRecorder captures every value recorded through this package's
// duration histograms for the lifetime of the test binary.
//
// OTel's global meter only delegates a package-level instrument (created
// once, at package init, via meter.Float64Histogram) to a real provider the
// first time one is installed with otel.SetMeterProvider — a later call
// does not rebind an instrument that has already resolved. Tests that need
// to read back recorded values must therefore share one recMeterProvider,
// installed exactly once here, rather than each calling SetMeterProvider
// with its own recorder.
var durationRecorder = &recorder{}

// spanNameRecorder captures every span name this package's shared `tracer`
// is asked to Start, for the same reason durationRecorder exists: the
// package-level tracer only delegates to a real TracerProvider the first
// time one is installed via otel.SetTracerProvider, so tests that need to
// read back span names must share one instance, installed exactly once here.
var spanNameRecorder = &spanRecorder{}

func TestMain(m *testing.M) {
	otelapi.SetMeterProvider(recMeterProvider{rec: durationRecorder})
	otelapi.SetTracerProvider(recTracerProvider{rec: spanNameRecorder})
	os.Exit(m.Run())
}

// spanRecorder captures the name of every span started through a
// recTracerProvider, for tests that need to assert on the actual span name a
// tracer.Start call used.
type spanRecorder struct {
	mu    sync.Mutex
	names []string
	attrs []attribute.Set // attributes each span was started with, by index
}

func (r *spanRecorder) add(name string, attrs []attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	r.attrs = append(r.attrs, attribute.NewSet(attrs...))
}

// recSpan is a span started through a recTracerProvider.
type recSpan struct {
	name  string
	attrs attribute.Set
}

// spansSince returns the spans recorded since from, as [spanRecorder.since]
// returns their names.
func (r *spanRecorder) spansSince(from int) []recSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recSpan, 0, len(r.names)-from)
	for i := from; i < len(r.names); i++ {
		out = append(out, recSpan{name: r.names[i], attrs: r.attrs[i]})
	}
	return out
}

// since returns the span names recorded since the call that produced from
// (typically len(r.names) captured before the operation under test ran).
func (r *spanRecorder) since(from int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.names)-from)
	copy(out, r.names[from:])
	return out
}

func (r *spanRecorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.names)
}

type recTracer struct {
	tracenoop.Tracer
	rec *spanRecorder
}

func (t recTracer) Start(ctx context.Context, spanName string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	t.rec.add(spanName, cfg.Attributes())
	return t.Tracer.Start(ctx, spanName, opts...)
}

// recTracerProvider is a [trace.TracerProvider] that records the name of
// every span it's asked to start, for tests to assert on afterward.
// Everything else it produces is a no-op.
type recTracerProvider struct {
	traceembedded.TracerProvider
	rec *spanRecorder
}

func (p recTracerProvider) Tracer(_ string, _ ...trace.TracerOption) trace.Tracer {
	return recTracer{rec: p.rec}
}

// recorder captures histogram values and counter increments recorded
// through a recMeterProvider, for tests that need to assert on the actual
// value a *Duration.Record or counter Add call writes rather than just that
// it was called. It also keeps the callbacks registered for observable
// instruments, so tests can collect them.
type histRecord struct {
	name  string
	value float64
	attrs attribute.Set
}

type recorder struct {
	mu        sync.Mutex
	records   []histRecord
	callbacks []metric.Callback
}

func (r *recorder) add(name string, v float64, attrs ...attribute.KeyValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, histRecord{name: name, value: v, attrs: attribute.NewSet(attrs...)})
}

// recordsFor returns every value recorded for the instrument name, with its
// attributes.
func (r *recorder) recordsFor(name string) []histRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []histRecord
	for _, rec := range r.records {
		if rec.name == name {
			out = append(out, rec)
		}
	}
	return out
}

// collect runs every registered callback and returns what they observed.
func (r *recorder) collect(ctx context.Context) []histRecord {
	r.mu.Lock()
	callbacks := append([]metric.Callback{}, r.callbacks...)
	r.mu.Unlock()
	o := &recObserver{}
	for _, cb := range callbacks {
		_ = cb(ctx, o) // the callbacks under test never fail
	}
	return o.records
}

func (r *recorder) valuesFor(name string) []float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]float64, 0, len(r.records))
	for _, rec := range r.records {
		if rec.name == name {
			out = append(out, rec.value)
		}
	}
	return out
}

type recHistogram struct {
	metricnoop.Float64Histogram
	name string
	rec  *recorder
}

func (h recHistogram) Record(_ context.Context, v float64, opts ...metric.RecordOption) {
	set := metric.NewRecordConfig(opts).Attributes()
	h.rec.add(h.name, v, set.ToSlice()...)
}

type recCounter struct {
	metricnoop.Int64Counter
	name string
	rec  *recorder
}

func (c recCounter) Add(_ context.Context, v int64, opts ...metric.AddOption) {
	set := metric.NewAddConfig(opts).Attributes()
	c.rec.add(c.name, float64(v), set.ToSlice()...)
}

// recGauge and recObservableCounter carry their name, so recObserver can
// tell which instrument an observation is for.
type recGauge struct {
	metricnoop.Int64ObservableGauge
	name string
}

type recObservableCounter struct {
	metricnoop.Int64ObservableCounter
	name string
}

// recObserver records the observations a callback makes.
type recObserver struct {
	metricembedded.Observer
	records []histRecord
}

func (o *recObserver) ObserveFloat64(metric.Float64Observable, float64, ...metric.ObserveOption) {}

func (o *recObserver) ObserveInt64(inst metric.Int64Observable, v int64, opts ...metric.ObserveOption) {
	// Instruments created before the provider was installed reach the
	// callback wrapped by the global meter.
	if u, ok := inst.(interface{ Unwrap() metric.Observable }); ok {
		if unwrapped, ok := u.Unwrap().(metric.Int64Observable); ok {
			inst = unwrapped
		}
	}
	var name string
	switch inst := inst.(type) {
	case recGauge:
		name = inst.name
	case recObservableCounter:
		name = inst.name
	}
	set := metric.NewObserveConfig(opts).Attributes()
	o.records = append(o.records, histRecord{name: name, value: float64(v), attrs: set})
}

type recMeter struct {
	metricnoop.Meter
	rec *recorder
}

func (m recMeter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return recHistogram{name: name, rec: m.rec}, nil
}

func (m recMeter) Int64Counter(name string, _ ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return recCounter{name: name, rec: m.rec}, nil
}

func (m recMeter) Int64ObservableGauge(name string, _ ...metric.Int64ObservableGaugeOption) (metric.Int64ObservableGauge, error) {
	return recGauge{name: name}, nil
}

func (m recMeter) Int64ObservableCounter(name string, _ ...metric.Int64ObservableCounterOption) (metric.Int64ObservableCounter, error) {
	return recObservableCounter{name: name}, nil
}

func (m recMeter) RegisterCallback(f metric.Callback, _ ...metric.Observable) (metric.Registration, error) {
	m.rec.mu.Lock()
	defer m.rec.mu.Unlock()
	m.rec.callbacks = append(m.rec.callbacks, f)
	return metricnoop.Meter{}.RegisterCallback(f)
}

// recMeterProvider is a [metric.MeterProvider] that records every
// Float64Histogram value and Int64Counter increment it's asked to record,
// and keeps every registered callback, for tests to assert on afterward.
// Everything else it produces is a no-op.
type recMeterProvider struct {
	metricembedded.MeterProvider
	rec *recorder
}

func (p recMeterProvider) Meter(_ string, _ ...metric.MeterOption) metric.Meter {
	return recMeter{rec: p.rec}
}
