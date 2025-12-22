package eventsourcing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

// Define constants for context keys
const (
	streamIDKey      ctxKey = "streamID"
	aggregateIDKey   ctxKey = "aggregateID"
	eventIDKey       ctxKey = "eventID"
	versionKey       ctxKey = "version"
	globalVersionKey ctxKey = "global_version"
	occurredAtKey    ctxKey = "occurredAt"
	metadataKey      ctxKey = "metadata"
)

// WithEnvelope adds the context of the Event to the context
func WithEnvelope(ctx context.Context, env *Envelope) context.Context {
	ctx = context.WithValue(ctx, streamIDKey, env.StreamID)
	ctx = context.WithValue(ctx, aggregateIDKey, env.Event.AggregateID())
	ctx = context.WithValue(ctx, eventIDKey, env.EventID)
	ctx = context.WithValue(ctx, versionKey, env.Version)
	ctx = context.WithValue(ctx, globalVersionKey, env.GlobalVersion)
	ctx = context.WithValue(ctx, occurredAtKey, env.OccurredAt)
	ctx = context.WithValue(ctx, metadataKey, env.Metadata)
	return ctx
}

// StreamIDFromContext returns the StreamID or "" if not present
func AggregateIDFromContext(ctx context.Context) string {
	if v := ctx.Value(streamIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// StreamIDFromContext returns the StreamID or "" if not present
func StreamIDFromContext(ctx context.Context) string {
	if v := ctx.Value(streamIDKey); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// EventIDFromContext returns the EventID or uuid.Nil if not present
func EventIDFromContext(ctx context.Context) uuid.UUID {
	if v := ctx.Value(eventIDKey); v != nil {
		if id, ok := v.(uuid.UUID); ok {
			return id
		}
	}
	return uuid.Nil
}

// VersionFromContext returns the Version or 0 if not present
func VersionFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(versionKey); v != nil {
		if ver, ok := v.(uint64); ok {
			return ver
		}
	}
	return 0
}

// VersionFromContext returns the Version or 0 if not present
func GlobalVersionFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(versionKey); v != nil {
		if ver, ok := v.(uint64); ok {
			return ver
		}
	}
	return 0
}

// OccurredAtFromContext returns OccurredAt or zero time if not present
func OccurredAtFromContext(ctx context.Context) time.Time {
	if v := ctx.Value(occurredAtKey); v != nil {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	return time.Time{}
}

// MetadataFromContext returns Metadata or nil if not present
func MetadataFromContext(ctx context.Context) map[string]any {
	if v := ctx.Value(metadataKey); v != nil {
		if md, ok := v.(map[string]any); ok {
			return md
		}
	}
	return nil
}
