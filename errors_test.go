package eventsourcing

import (
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
