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
		StreamID:       "stream-123",
		Event:          &event{aggregateID: "agg-456"},
		EventID:        eventID,
		Version:        7,
		GlobalPosition: 42,
		OccurredAt:     occurredAt,
		Metadata:       metadata,
		CorrelationID:  "corr-123",
		CausationID:    "cause-of-env",
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
			want: "agg-456",
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
			name: "GlobalPositionFromContext with value",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return GlobalPositionFromContext(ctx) },
			want: uint64(42),
		},
		{
			name: "GlobalPositionFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return GlobalPositionFromContext(ctx) },
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
		{
			name: "CausationIDFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return CausationIDFromContext(ctx) },
			want: "",
		},
		{
			// Work under the envelope is caused by the envelope's event,
			// not by whatever caused that event.
			name: "CausationIDFromContext is the handled event's ID",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return CausationIDFromContext(ctx) },
			want: eventID.String(),
		},
		{
			name: "CorrelationIDFromContext without value",
			ctx:  emptyCtx,
			fn:   func(ctx context.Context) any { return CorrelationIDFromContext(ctx) },
			want: "",
		},
		{
			name: "CorrelationIDFromContext is the handled event's chain",
			ctx:  ctxWithEnv,
			fn:   func(ctx context.Context) any { return CorrelationIDFromContext(ctx) },
			want: "corr-123",
		},
		{
			name: "CorrelationIDFromContext of an event stored without one",
			ctx:  WithEnvelope(t.Context(), &Envelope{EventID: eventID, Event: &event{}}),
			fn:   func(ctx context.Context) any { return CorrelationIDFromContext(ctx) },
			want: eventID.String(),
		},
		{
			name: "WithCorrelationID and WithCausationID override",
			ctx:  WithCausationID(WithCorrelationID(ctxWithEnv, "request-1"), "cause-1"),
			fn: func(ctx context.Context) any {
				return CorrelationIDFromContext(ctx) + "/" + CausationIDFromContext(ctx)
			},
			want: "request-1/cause-1",
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

// TestWithEnvelope_NilEvent is a regression test for GitHub issue #60:
// WithEnvelope called env.Event.AggregateID() with no nil check, panicking
// on an Envelope whose Event is nil, unlike every other *FromContext getter
// in this file, which degrades gracefully to a documented zero value.
func TestWithEnvelope_NilEvent(t *testing.T) {
	env := &Envelope{
		StreamID: "some-stream",
		Event:    nil,
	}

	ctx := WithEnvelope(t.Context(), env)

	if got := AggregateIDFromContext(ctx); got != "" {
		t.Errorf("AggregateIDFromContext() = %q, want \"\"", got)
	}
	if got := StreamIDFromContext(ctx); got != "some-stream" {
		t.Errorf("StreamIDFromContext() = %q, want %q", got, "some-stream")
	}
}
