package otel

import (
	"context"
	"os"
	"sync"
	"testing"

	otelapi "go.opentelemetry.io/otel"
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
}

func (r *spanRecorder) add(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
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
	t.rec.add(spanName)
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

// recorder captures histogram values recorded through a recMeterProvider,
// for tests that need to assert on the actual value a *Duration.Record call
// writes rather than just that it was called.
type histRecord struct {
	name  string
	value float64
}

type recorder struct {
	mu      sync.Mutex
	records []histRecord
}

func (r *recorder) add(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, histRecord{name: name, value: v})
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

func (h recHistogram) Record(_ context.Context, v float64, _ ...metric.RecordOption) {
	h.rec.add(h.name, v)
}

type recMeter struct {
	metricnoop.Meter
	rec *recorder
}

func (m recMeter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return recHistogram{name: name, rec: m.rec}, nil
}

// recMeterProvider is a [metric.MeterProvider] that records every
// Float64Histogram value it's asked to record, for tests to assert on
// afterward. Everything else it produces is a no-op.
type recMeterProvider struct {
	metricembedded.MeterProvider
	rec *recorder
}

func (p recMeterProvider) Meter(_ string, _ ...metric.MeterOption) metric.Meter {
	return recMeter{rec: p.rec}
}
