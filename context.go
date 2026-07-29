package eventsourcing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

const (
	streamIDKey      ctxKey = "streamID"
	aggregateIDKey   ctxKey = "aggregateID"
	eventIDKey       ctxKey = "eventID"
	versionKey       ctxKey = "version"
	globalVersionKey ctxKey = "global_version"
	occurredAtKey    ctxKey = "occurredAt"
	metadataKey      ctxKey = "metadata"
	causationIDKey   ctxKey = "causationID"
)

// WithEnvelope returns a copy of ctx carrying env's stream ID, aggregate ID,
// event ID, version, global version, occurred-at time, and metadata,
// retrievable via the *FromContext functions below. It does not carry a
// causation ID; use [WithCausation] for that. The aggregate ID is "" if
// env.Event is nil.
func WithEnvelope(ctx context.Context, env *Envelope) context.Context {
	var aggregateID string
	if env.Event != nil {
		aggregateID = env.Event.AggregateID()
	}

	ctx = context.WithValue(ctx, streamIDKey, env.StreamID)
	ctx = context.WithValue(ctx, aggregateIDKey, aggregateID)
	ctx = context.WithValue(ctx, eventIDKey, env.EventID)
	ctx = context.WithValue(ctx, versionKey, env.Version)
	ctx = context.WithValue(ctx, globalVersionKey, env.GlobalVersion)
	ctx = context.WithValue(ctx, occurredAtKey, env.OccurredAt)
	ctx = context.WithValue(ctx, metadataKey, env.Metadata)
	return ctx
}

// AggregateIDFromContext returns the aggregate ID set by [WithEnvelope], or
// "" if ctx carries none.
func AggregateIDFromContext(ctx context.Context) string {
	if v := ctx.Value(aggregateIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// StreamIDFromContext returns the stream ID set by [WithEnvelope], or "" if
// ctx carries none.
func StreamIDFromContext(ctx context.Context) string {
	if v := ctx.Value(streamIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// EventIDFromContext returns the event ID set by [WithEnvelope], or
// [uuid.Nil] if ctx carries none.
func EventIDFromContext(ctx context.Context) uuid.UUID {
	if v := ctx.Value(eventIDKey); v != nil {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

// VersionFromContext returns the stream version set by [WithEnvelope], or 0
// if ctx carries none.
func VersionFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(versionKey); v != nil {
		if ver, ok := v.(uint64); ok {
			return ver
		}
	}
	return 0
}

// GlobalVersionFromContext returns the global version set by
// [WithEnvelope], or 0 if ctx carries none.
func GlobalVersionFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(globalVersionKey); v != nil {
		if ver, ok := v.(uint64); ok {
			return ver
		}
	}
	return 0
}

// OccurredAtFromContext returns the occurred-at time set by [WithEnvelope],
// or the zero [time.Time] if ctx carries none.
func OccurredAtFromContext(ctx context.Context) time.Time {
	if v := ctx.Value(occurredAtKey); v != nil {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	return time.Time{}
}

// MetadataFromContext returns the metadata set by [WithEnvelope], or nil if
// ctx carries none.
func MetadataFromContext(ctx context.Context) map[string]any {
	if v := ctx.Value(metadataKey); v != nil {
		if md, ok := v.(map[string]any); ok {
			return md
		}
	}
	return nil
}

// WithCausation returns a copy of ctx carrying causation, the identifier of
// whatever caused the work now happening under ctx — for example, a
// command's type name — for handlers to attach to events or log entries.
func WithCausation(ctx context.Context, causation string) context.Context {
	ctx = context.WithValue(ctx, causationIDKey, causation)
	return ctx
}

// CausationFromContext returns the causation ID set by [WithCausation], or
// "" if ctx carries none.
func CausationFromContext(ctx context.Context) string {
	if v := ctx.Value(causationIDKey); v != nil {
		if causation, ok := v.(string); ok {
			return causation
		}
	}
	return ""
}
