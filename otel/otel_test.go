package otel

import (
	"context"
	"os"
	"sync"
	"testing"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/embedded"
	"go.opentelemetry.io/otel/metric/noop"
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

func TestMain(m *testing.M) {
	otelapi.SetMeterProvider(recMeterProvider{rec: durationRecorder})
	os.Exit(m.Run())
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
	noop.Float64Histogram
	name string
	rec  *recorder
}

func (h recHistogram) Record(_ context.Context, v float64, _ ...metric.RecordOption) {
	h.rec.add(h.name, v)
}

type recMeter struct {
	noop.Meter
	rec *recorder
}

func (m recMeter) Float64Histogram(name string, _ ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return recHistogram{name: name, rec: m.rec}, nil
}

// recMeterProvider is a [metric.MeterProvider] that records every
// Float64Histogram value it's asked to record, for tests to assert on
// afterward. Everything else it produces is a no-op.
type recMeterProvider struct {
	embedded.MeterProvider
	rec *recorder
}

func (p recMeterProvider) Meter(_ string, _ ...metric.MeterOption) metric.Meter {
	return recMeter{rec: p.rec}
}
