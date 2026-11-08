package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/codes"
)

type durationTestQuery struct{}

func (durationTestQuery) ID() []byte { return []byte("q-1") }

// TestQueryTelemetryDurationRecordedInSeconds is a regression test for
// GitHub issue #57: QueryTelemetry recorded
// time.Since(startTime).Milliseconds() into eventsourcing.query.duration,
// but the instrument declares metric.WithUnit("s") and second-scaled bucket
// boundaries — every sample from the middleware path was 1000x too large,
// and any operation faster than 1ms recorded as exactly 0 (Milliseconds()
// truncates to an integer). WithQueryTelemetry was already correct, using
// .Seconds().
func TestQueryTelemetryDurationRecordedInSeconds(t *testing.T) {
	rec := durationRecorder

	const work = 50 * time.Millisecond
	const instrument = "eventsourcing.query.duration"

	tests := []struct {
		name   string
		invoke func(t *testing.T)
	}{
		{
			name: "QueryTelemetry middleware",
			invoke: func(t *testing.T) {
				h := QueryTelemetry()(func(ctx context.Context, qry eventsourcing.Query) (any, error) {
					time.Sleep(work)
					return "ok", nil
				})
				if _, err := h(context.Background(), durationTestQuery{}); err != nil {
					t.Fatalf("query: %v", err)
				}
			},
		},
		{
			name: "WithQueryTelemetry decorator",
			invoke: func(t *testing.T) {
				h := WithQueryTelemetry(eventsourcing.NewQueryHandlerFunc(
					func(ctx context.Context, qry durationTestQuery) (string, error) {
						time.Sleep(work)
						return "ok", nil
					}))
				if _, err := h.HandleQuery(context.Background(), durationTestQuery{}); err != nil {
					t.Fatalf("query: %v", err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(rec.valuesFor(instrument))
			tc.invoke(t)
			got := rec.valuesFor(instrument)
			if len(got) != before+1 {
				t.Fatalf("expected exactly one new %s sample, got %d", instrument, len(got)-before)
			}

			v := got[len(got)-1]
			if v >= 1 {
				t.Errorf("%s recorded %v for a %v operation; the instrument declares unit \"s\", so it should be ~%v",
					instrument, v, work, work.Seconds())
			}
			if v < work.Seconds()*0.5 {
				t.Errorf("%s recorded %v for a %v operation; expected ~%v seconds",
					instrument, v, work, work.Seconds())
			}
		})
	}
}

// TestQueryTelemetry_SpanName covers the span name, the same through the
// middleware and the wrapper, for a value or pointer query and for a
// handler over an interface: "handle" and the query's type, without a
// pointer.
func TestQueryTelemetry_SpanName(t *testing.T) {
	const want = "handle telemetry.durationTestQuery"
	gateway := QueryTelemetry()(func(context.Context, eventsourcing.Query) (any, error) { return nil, nil })
	forms := map[string]func(){
		"QueryTelemetry value":   func() { _, _ = gateway(context.Background(), durationTestQuery{}) },
		"QueryTelemetry pointer": func() { _, _ = gateway(context.Background(), &durationTestQuery{}) },
		"WithQueryTelemetry": func() {
			_, _ = WithQueryTelemetry(eventsourcing.NewQueryHandlerFunc(func(context.Context, durationTestQuery) (int, error) {
				return 0, nil
			})).HandleQuery(context.Background(), durationTestQuery{})
		},
		"WithQueryTelemetry interface": func() {
			_, _ = WithQueryTelemetry(eventsourcing.NewQueryHandlerFunc(func(context.Context, eventsourcing.Query) (int, error) {
				return 0, nil
			})).HandleQuery(context.Background(), durationTestQuery{})
		},
		"WithQueryTelemetry pointer": func() {
			_, _ = WithQueryTelemetry(eventsourcing.NewQueryHandlerFunc(func(context.Context, *durationTestQuery) (int, error) {
				return 0, nil
			})).HandleQuery(context.Background(), &durationTestQuery{})
		},
	}
	for form, call := range forms {
		t.Run(form, func(t *testing.T) {
			before := spanNameRecorder.len()
			call()
			if got := spanNameRecorder.since(before); len(got) != 1 || got[0] != want {
				t.Errorf("spans = %v, want [%s]", got, want)
			}
		})
	}
}

// TestQueryTelemetry_SpanStatus covers the span status: a success leaves
// it unset, as the OpenTelemetry Trace API asks of instrumentation, and an
// error sets it to Error.
func TestQueryTelemetry_SpanStatus(t *testing.T) {
	tests := map[string]struct {
		err  error
		want codes.Code
	}{
		"success": {nil, codes.Unset},
		"failure": {errors.New("boom"), codes.Error},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			handler := WithQueryTelemetry(eventsourcing.NewQueryHandlerFunc(func(context.Context, durationTestQuery) (int, error) {
				return 0, tt.err
			}))
			before := spanNameRecorder.len()
			_, _ = handler.HandleQuery(context.Background(), durationTestQuery{})
			spans := spanNameRecorder.spansSince(before)
			if len(spans) != 1 || spans[0].status != tt.want {
				t.Errorf("spans = %+v, want one with status %v", spans, tt.want)
			}
		})
	}
}
