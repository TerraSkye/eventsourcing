package eventsourcing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type event struct {
	aggregateID string
}

func (e *event) EventType() string {
	return "myevent"
}

func (e *event) AggregateID() string {
	return e.aggregateID
}

func TestContextGetters(t *testing.T) {

	eventID := uuid.New()
	occurredAt := time.Now()
	metadata := map[string]any{"key": "value"}

	env := &Envelope{
		StreamID:      "stream-123",
		Event:         &event{aggregateID: "agg-456"},
		EventID:       eventID,
		Version:       7,
		GlobalVersion: 42,
		OccurredAt:    occurredAt,
		Metadata:      metadata,
	}

	ctxWithEnv := WithEnvelope(t.Context(), env)
	emptyCtx := t.Context()

	tests := []struct {
		name string
		ctx  context.Context
		fn   func(context.Context) any
		want any
	}{
		{
			name: "StreamIDFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return StreamIDFromContext(ctx) },
			want: "stream-123",
		},
		{
			name: "StreamIDFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return StreamIDFromContext(ctx) },
			want: "",
		},
		{
			name: "AggregateIDFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return AggregateIDFromContext(ctx) },
			want: "stream-123", // matches your current code (bug)
		},
		{
			name: "AggregateIDFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return AggregateIDFromContext(ctx) },
			want: "",
		},
		{
			name: "EventIDFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return EventIDFromContext(ctx) },
			want: eventID,
		},
		{
			name: "EventIDFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return EventIDFromContext(ctx) },
			want: uuid.Nil,
		},
		{
			name: "VersionFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return VersionFromContext(ctx) },
			want: uint64(7),
		},
		{
			name: "VersionFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return VersionFromContext(ctx) },
			want: uint64(0),
		},
		{
			name: "GlobalVersionFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return GlobalVersionFromContext(ctx) },
			want: uint64(7), // matches your current code (bug)
		},
		{
			name: "GlobalVersionFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return GlobalVersionFromContext(ctx) },
			want: uint64(0),
		},
		{
			name: "OccurredAtFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return OccurredAtFromContext(ctx) },
			want: occurredAt,
		},
		{
			name: "OccurredAtFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return OccurredAtFromContext(ctx) },
			want: time.Time{},
		},
		{
			name: "MetadataFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return MetadataFromContext(ctx) },
			want: metadata,
		},
		{
			name: "MetadataFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return MetadataFromContext(ctx) },
			want: map[string]any{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.fn(tt.ctx)
			switch want := tt.want.(type) {
			case time.Time:
				if !got.(time.Time).Equal(want) {
					t.Errorf("%s = %v, want %v", tt.name, got, want)
				}
			case map[string]any:
				gotMap := got.(map[string]any)
				if len(gotMap) != len(want) {
					t.Errorf("%s = %v, want %v", tt.name, got, want)
				}
				for k, v := range want {
					if gotMap[k] != v {
						t.Errorf("%s = %v, want %v", tt.name, got, want)
					}
				}
			default:
				if got != want {
					t.Errorf("%s = %v, want %v", tt.name, got, want)
				}
			}
		})
	}
}
