package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"go.opentelemetry.io/otel/attribute"
)

type panicProbeCmd struct{}

func (panicProbeCmd) AggregateID() string { return "agg-1" }

func (panicProbeCmd) CommandType() string { return "panicProbeCmd" }

// TestCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic is a regression
// test for GitHub issue #25: a decide function that signals a business rule
// violation via a bare &eventsourcing.BusinessRuleViolationError{} (no inner
// cause) used to crash CommandTelemetry, instead of being recorded as a
// graceful, expected rejection.
func TestCommandTelemetry_ZeroValueBusinessViolationDoesNotPanic(t *testing.T) {
	mw := CommandTelemetry()

	next := func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, &eventsourcing.BusinessRuleViolationError{}
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
		return eventsourcing.AppendResult{}, &eventsourcing.BusinessRuleViolationError{}
	})

	wrapped := WithCommandTelemetry(next)

	_, err := wrapped(context.Background(), panicProbeCmd{})
	if err == nil {
		t.Fatalf("expected the business rule violation to be returned")
	}
}

type raceProbeCmd struct{ id string }

func (c raceProbeCmd) AggregateID() string { return c.id }

func (c raceProbeCmd) CommandType() string { return "raceProbeCmd" }

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

func (durationTestCommand) CommandType() string { return "durationTestCommand" }

// TestCommandTelemetryDurationRecordedInSeconds is a regression test for
// GitHub issue #57: CommandTelemetry recorded
// time.Since(startTime).Milliseconds() into eventsourcing.command.duration,
// but the instrument declares metric.WithUnit("s") and second-scaled bucket
// boundaries — every sample from the middleware path was 1000x too large,
// and any operation faster than 1ms recorded as exactly 0 (Milliseconds()
// truncates to an integer). WithCommandTelemetry was already correct, using
// .Seconds().
func TestCommandTelemetryDurationRecordedInSeconds(t *testing.T) {
	rec := durationRecorder

	const work = 50 * time.Millisecond
	const instrument = "eventsourcing.command.duration"

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

// TestCommandTelemetry_ResultLabels covers the result CommandDuration records
// for each outcome, through both the middleware and the wrapper: a business
// rule violation is "rejected", apart from a "failure".
func TestCommandTelemetry_ResultLabels(t *testing.T) {
	const instrument = "eventsourcing.command.duration"
	outcomes := []struct {
		name string
		err  error
		want string
	}{
		{"success", nil, "success"},
		{"business rule violation", fmt.Errorf("decide: %w", eventsourcing.NewBusinessRuleViolation(errors.New("already done"))), "rejected"},
		{"conflict", &eventsourcing.StreamRevisionConflictError{Stream: "agg-1"}, "failure"},
		{"store error", errors.New("connection refused"), "failure"},
	}
	for _, o := range outcomes {
		forms := map[string]func() error{
			"CommandTelemetry": func() error {
				_, err := CommandTelemetry()(func(context.Context, eventsourcing.Command) (eventsourcing.AppendResult, error) {
					return eventsourcing.AppendResult{}, o.err
				})(context.Background(), panicProbeCmd{})
				return err
			},
			"WithCommandTelemetry": func() error {
				_, err := WithCommandTelemetry(func(context.Context, panicProbeCmd) (eventsourcing.AppendResult, error) {
					return eventsourcing.AppendResult{}, o.err
				})(context.Background(), panicProbeCmd{})
				return err
			},
		}
		for form, call := range forms {
			t.Run(o.name+"/"+form, func(t *testing.T) {
				from := len(durationRecorder.recordsFor(instrument))
				if err := call(); !errors.Is(err, o.err) {
					t.Fatalf("error = %v, want %v", err, o.err)
				}
				got := durationRecorder.recordsFor(instrument)[from:]
				if len(got) != 1 {
					t.Fatalf("recorded %d %s values, want 1", len(got), instrument)
				}
				if v, _ := got[0].attrs.Value(AttrResult); v.AsString() != o.want {
					t.Errorf("%s = %q, want %q", AttrResult, v.AsString(), o.want)
				}
			})
		}
	}
}

// TestCommandTelemetry_SpanName covers the span name, the same through the
// middleware and the wrapper: "handle" and the command type.
func TestCommandTelemetry_SpanName(t *testing.T) {
	const want = "handle durationTestCommand"
	forms := map[string]func(){
		"CommandTelemetry": func() {
			_, _ = CommandTelemetry()(func(context.Context, eventsourcing.Command) (eventsourcing.AppendResult, error) {
				return eventsourcing.AppendResult{}, nil
			})(context.Background(), durationTestCommand{})
		},
		"WithCommandTelemetry": func() {
			_, _ = WithCommandTelemetry(func(context.Context, durationTestCommand) (eventsourcing.AppendResult, error) {
				return eventsourcing.AppendResult{}, nil
			})(context.Background(), durationTestCommand{})
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
