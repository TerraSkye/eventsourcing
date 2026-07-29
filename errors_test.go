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
			name: "StreamRevisionConflictError with non-Revision StreamStates",
			err: StreamRevisionConflictError{
				Stream:           "stream-123",
				ExpectedRevision: Any{},
				ActualRevision:   StreamExists{},
			},
			want: `concurrency conflict on stream "stream-123": (expected version -1, actual -2)`,
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
	err := NewBusinessRuleViolation(inner)

	want := "business rule violation :insufficient balance"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrBusinessRuleViolation_Error_NilCause(t *testing.T) {
	err := ErrBusinessRuleViolation{}

	want := "business rule violation"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrBusinessRuleViolation_Unwrap(t *testing.T) {
	inner := errors.New("item out of stock")
	err := NewBusinessRuleViolation(inner)

	if !errors.Is(err, inner) {
		t.Error("errors.Is should match the wrapped inner error")
	}
}

func TestErrBusinessRuleViolation_Cause(t *testing.T) {
	inner := errors.New("duplicate order")

	var violation *ErrBusinessRuleViolation
	if !errors.As(NewBusinessRuleViolation(inner), &violation) {
		t.Fatal("expected NewBusinessRuleViolation to return an *ErrBusinessRuleViolation")
	}

	if violation.Cause() != inner {
		t.Errorf("Cause() = %v, want %v", violation.Cause(), inner)
	}
}

func TestErrBusinessRuleViolation_ErrorsAs(t *testing.T) {
	inner := errors.New("age restriction")
	wrapped := fmt.Errorf("command failed: %w", NewBusinessRuleViolation(inner))

	var violation *ErrBusinessRuleViolation
	if !errors.As(wrapped, &violation) {
		t.Fatal("errors.As should unwrap to *ErrBusinessRuleViolation")
	}

	if violation.Cause() != inner {
		t.Errorf("Cause() = %v, want %v", violation.Cause(), inner)
	}
}

// TestNewBusinessRuleViolation_NilErr covers the reason NewBusinessRuleViolation
// exists: passing a possibly-nil err straight through must produce a true nil
// error, not a non-nil interface wrapping a nil-cause *ErrBusinessRuleViolation
// (the classic typed-nil-in-interface footgun), so a decide function can
// return NewBusinessRuleViolation(validate(...)) unconditionally.
func TestNewBusinessRuleViolation_NilErr(t *testing.T) {
	if err := NewBusinessRuleViolation(nil); err != nil {
		t.Errorf("NewBusinessRuleViolation(nil) = %v, want nil", err)
	}
}
