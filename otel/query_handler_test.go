package otel

import (
	"context"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
)

type durationTestQuery struct{}

func (durationTestQuery) ID() []byte { return []byte("q-1") }

// TestQueryTelemetryDurationRecordedInSeconds is a regression test for
// GitHub issue #57: QueryTelemetry recorded
// time.Since(startTime).Milliseconds() into eventsourcing.queries.duration,
// but the instrument declares metric.WithUnit("s") and second-scaled bucket
// boundaries — every sample from the middleware path was 1000x too large,
// and any operation faster than 1ms recorded as exactly 0 (Milliseconds()
// truncates to an integer). WithQueryTelemetry was already correct, using
// .Seconds().
func TestQueryTelemetryDurationRecordedInSeconds(t *testing.T) {
	rec := durationRecorder

	const work = 50 * time.Millisecond
	const instrument = "eventsourcing.queries.duration"

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
