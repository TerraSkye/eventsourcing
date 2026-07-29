package otel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
)

type panicProbeCmd struct{}

func (panicProbeCmd) AggregateID() string { return "agg-1" }

// TestCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic is a regression
// test for GitHub issue #25: a decide function that signals a business rule
// violation via a bare &eventsourcing.ErrBusinessRuleViolation{} (no inner
// cause) used to crash CommandTelemetry, instead of being recorded as a
// graceful, expected rejection.
func TestCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic(t *testing.T) {
	mw := CommandTelemetry()

	next := func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, &eventsourcing.ErrBusinessRuleViolation{}
	}

	wrapped := mw(next)

	_, err := wrapped(context.Background(), panicProbeCmd{})
	if err == nil {
		t.Fatalf("expected the business rule violation to be returned")
	}
}

// TestWithCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic is the
// WithCommandTelemetry counterpart of
// TestCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic.
func TestWithCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic(t *testing.T) {
	next := eventsourcing.CommandHandler[panicProbeCmd](func(ctx context.Context, cmd panicProbeCmd) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, &eventsourcing.ErrBusinessRuleViolation{}
	})

	wrapped := WithCommandTelemetry(next)

	_, err := wrapped(context.Background(), panicProbeCmd{})
	if err == nil {
		t.Fatalf("expected the business rule violation to be returned")
	}
}

type raceProbeCmd struct{ id string }

func (c raceProbeCmd) AggregateID() string { return c.id }

// TestWithCommandTelemetry_ConcurrentCallsRaceOnSharedBaseAttributes is a
// regression test for GitHub issue #59: baseAttributes was built once,
// outside the returned handler closure, and every call appended directly to
// it. Once cfg.Attributes was large enough to leave baseAttributes with
// spare capacity (11 static attributes reproduces it reliably), concurrent
// calls raced on its shared backing array instead of each getting an
// independent slice.
func TestWithCommandTelemetry_ConcurrentCallsRaceOnSharedBaseAttributes(t *testing.T) {
	extra := make([]attribute.KeyValue, 11)
	for i := range extra {
		extra[i] = attribute.String(fmt.Sprintf("k%d", i), "v")
	}

	next := func(ctx context.Context, cmd raceProbeCmd) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{StreamID: cmd.id}, nil
	}

	handler := WithCommandTelemetry[raceProbeCmd](next, WithAttributes(extra...))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = handler(context.Background(), raceProbeCmd{id: fmt.Sprintf("agg-%d", i)})
		}(i)
	}
	wg.Wait()
}

type durationTestCommand struct{}

func (durationTestCommand) AggregateID() string { return "agg-1" }

// TestCommandTelemetryDurationRecordedInSeconds is a regression test for
// GitHub issue #57: CommandTelemetry recorded
// time.Since(startTime).Milliseconds() into eventsourcing.commands.duration,
// but the instrument declares metric.WithUnit("s") and second-scaled bucket
// boundaries — every sample from the middleware path was 1000x too large,
// and any operation faster than 1ms recorded as exactly 0 (Milliseconds()
// truncates to an integer). WithCommandTelemetry was already correct, using
// .Seconds().
func TestCommandTelemetryDurationRecordedInSeconds(t *testing.T) {
	rec := durationRecorder

	const work = 50 * time.Millisecond
	const instrument = "eventsourcing.commands.duration"

	tests := []struct {
		name   string
		invoke func(t *testing.T)
	}{
		{
			name: "CommandTelemetry middleware",
			invoke: func(t *testing.T) {
				h := CommandTelemetry()(func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
					time.Sleep(work)
					return eventsourcing.AppendResult{Successful: true}, nil
				})
				if _, err := h(context.Background(), durationTestCommand{}); err != nil {
					t.Fatalf("command: %v", err)
				}
			},
		},
		{
			name: "WithCommandTelemetry decorator",
			invoke: func(t *testing.T) {
				h := WithCommandTelemetry(func(ctx context.Context, cmd durationTestCommand) (eventsourcing.AppendResult, error) {
					time.Sleep(work)
					return eventsourcing.AppendResult{Successful: true}, nil
				})
				if _, err := h(context.Background(), durationTestCommand{}); err != nil {
					t.Fatalf("command: %v", err)
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
