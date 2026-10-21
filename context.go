package eventsourcing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type ctxKey string

const (
	streamIDKey       ctxKey = "streamID"
	aggregateIDKey    ctxKey = "aggregateID"
	eventIDKey        ctxKey = "eventID"
	versionKey        ctxKey = "version"
	globalPositionKey ctxKey = "global_position"
	occurredAtKey     ctxKey = "occurredAt"
	metadataKey       ctxKey = "metadata"
	correlationIDKey  ctxKey = "correlationID"
	causationIDKey    ctxKey = "causationID"
)

// WithEnvelope returns a copy of ctx carrying env's stream ID, aggregate ID,
// event ID, version, global position, occurred-at time, and metadata,
// retrievable via the *FromContext functions below. The aggregate ID is ""
// if env.Event is nil.
//
// It also puts the work done under ctx in env's chain: the causation ID
// becomes env's EventID and the correlation ID env's CorrelationID, so a
// command sent while handling env produces events caused by it. An event
// stored without a CorrelationID counts as the root of its chain, its own
// EventID the correlation. Every event bus and the projection runner call
// WithEnvelope before a handler runs.
func WithEnvelope(ctx context.Context, env *Envelope) context.Context {
	var aggregateID string
	if env.Event != nil {
		aggregateID = env.Event.AggregateID()
	}

	ctx = context.WithValue(ctx, streamIDKey, env.StreamID)
	ctx = context.WithValue(ctx, aggregateIDKey, aggregateID)
	ctx = context.WithValue(ctx, eventIDKey, env.EventID)
	ctx = context.WithValue(ctx, versionKey, env.Version)
	ctx = context.WithValue(ctx, globalPositionKey, env.GlobalPosition)
	ctx = context.WithValue(ctx, occurredAtKey, env.OccurredAt)
	ctx = context.WithValue(ctx, metadataKey, env.Metadata)

	var eventID, correlationID string
	if env.EventID != uuid.Nil {
		eventID = env.EventID.String()
	}
	correlationID = env.CorrelationID
	if correlationID == "" {
		correlationID = eventID
	}
	ctx = WithCorrelationID(ctx, correlationID)
	ctx = WithCausationID(ctx, eventID)
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

// GlobalPositionFromContext returns the global position set by
// [WithEnvelope], or 0 if ctx carries none.
func GlobalPositionFromContext(ctx context.Context) uint64 {
	if v := ctx.Value(globalPositionKey); v != nil {
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

// WithCorrelationID returns a copy of ctx carrying correlationID, the chain
// the work under ctx belongs to; see [Envelope.CorrelationID]. Use it to
// start a chain deliberately, for example from an API request's ID. Pass
// "" to start a new chain, which [NewCommandHandler] then names after the
// first event it appends.
func WithCorrelationID(ctx context.Context, correlationID string) context.Context {
	return context.WithValue(ctx, correlationIDKey, correlationID)
}

// CorrelationIDFromContext returns the correlation ID set by
// [WithCorrelationID] or [WithEnvelope], or "" if ctx carries none.
func CorrelationIDFromContext(ctx context.Context) string {
	if v := ctx.Value(correlationIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}

// WithCausationID returns a copy of ctx carrying causationID, the EventID of
// the event that caused the work under ctx; see [Envelope.CausationID].
// [WithEnvelope] sets it for every handled event, so call it only for work
// that event handling does not deliver, such as an automation's.
func WithCausationID(ctx context.Context, causationID string) context.Context {
	return context.WithValue(ctx, causationIDKey, causationID)
}

// CausationIDFromContext returns the causation ID set by [WithCausationID]
// or [WithEnvelope], or "" if ctx carries none.
func CausationIDFromContext(ctx context.Context) string {
	if v := ctx.Value(causationIDKey); v != nil {
		if id, ok := v.(string); ok {
			return id
		}
	}
	return ""
}
