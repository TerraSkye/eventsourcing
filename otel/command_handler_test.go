package otel

import (
	"context"
	"testing"

	"github.com/terraskye/eventsourcing"
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
