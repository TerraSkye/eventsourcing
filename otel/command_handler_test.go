package otel

import (
	"context"
	"fmt"
	"sync"
	"testing"

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
