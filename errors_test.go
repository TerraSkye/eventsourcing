package eventsourcing

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrorStrings(t *testing.T) {
	// Dummy Event type to satisfy ErrSkippedEvent

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "StreamRevisionConflictError",
			err: StreamRevisionConflictError{
				Stream:           "stream-123",
				ExpectedRevision: Revision(5),
				ActualRevision:   Revision(7),
			},
			want: `concurrency conflict on stream "stream-123": (expected version 5, actual 7)`,
		},
		{
			name: "ErrSkippedEvent",
			err:  ErrSkippedEvent{Event: &event{}},
			want: "skipped event of type *eventsourcing.event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.want {
				t.Errorf("Error() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestErrBusinessRuleViolation_Error(t *testing.T) {
	inner := errors.New("insufficient balance")
	err := ErrBusinessRuleViolation{Err: inner}

	want := "business rule violation :insufficient balance"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrBusinessRuleViolation_Unwrap(t *testing.T) {
	inner := errors.New("item out of stock")
	err := ErrBusinessRuleViolation{Err: inner}

	if !errors.Is(err, inner) {
		t.Error("errors.Is should match the wrapped inner error")
	}
}

func TestErrBusinessRuleViolation_Cause(t *testing.T) {
	inner := errors.New("duplicate order")
	err := ErrBusinessRuleViolation{Err: inner}

	if err.Cause() != inner {
		t.Errorf("Cause() = %v, want %v", err.Cause(), inner)
	}
}

func TestErrBusinessRuleViolation_ErrorsAs(t *testing.T) {
	inner := errors.New("age restriction")
	wrapped := fmt.Errorf("command failed: %w", &ErrBusinessRuleViolation{Err: inner})

	var violation *ErrBusinessRuleViolation
	if !errors.As(wrapped, &violation) {
		t.Fatal("errors.As should unwrap to *ErrBusinessRuleViolation")
	}

	if violation.Err != inner {
		t.Errorf("inner error = %v, want %v", violation.Err, inner)
	}
}
