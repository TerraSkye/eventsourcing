package eventsourcing

import (
	"context"
	"fmt"
	"time"
)

type ctxKey string

const (
	originalMessage ctxKey = "original_message"
)

// OriginalMessageFromCtx returns the original message that was received by the event/command handler.
func OriginalMessageFromCtx(ctx context.Context) *Message {
	val, ok := ctx.Value(originalMessage).(*Message)
	if !ok {
		return nil
	}
	return val
}

// CtxWithOriginalMessage returns a new context with the original message attached.
func CtxWithOriginalMessage(ctx context.Context, msg *Message) context.Context {
	return context.WithValue(ctx, originalMessage, msg)
}

// Metadata is sent with every message to provide extra context without unmarshaling the message payload.
type Metadata map[string]string

// Get returns the metadata value for the given key. If the key is not found, an empty string is returned.
func (m Metadata) Get(key string) string {
	if v, ok := m[key]; ok {
		return v
	}

	return ""
}

// Set sets the metadata key to value.
func (m Metadata) Set(key, value string) {
	m[key] = value
}

type Message struct {
	Payload    []byte
	Metadata   Metadata
	Version    uint64
	OccurredAt time.Time
}

// Define constants for context keys
const (
	AggregateVersionKey = "commanded.aggregate_version"
	CausationIdKey      = "commanded.causation_id"
	CorrelationIdKey    = "commanded.correlation_id"
	AggregateUUIDKey    = "commanded.aggregate_uuid"
	ApplicationKey      = "commanded.application"
	CommandKey          = "commanded.command"
	HandlerKey          = "commanded.handler"
)

// ExtractAggregateVersion extracts the aggregate version from the context.
func ExtractAggregateVersion(ctx context.Context) (string, error) {
	version, ok := ctx.Value(AggregateVersionKey).(string)
	if !ok {
		return "", fmt.Errorf("aggregate version not found in context")
	}
	return version, nil
}

// ExtractCausationId extracts the causation ID from the context.
func ExtractCausationId(ctx context.Context) (string, error) {
	causationId, ok := ctx.Value(CausationIdKey).(string)
	if !ok {
		return "", fmt.Errorf("causation ID not found in context")
	}
	return causationId, nil
}

// ExtractCorrelationId extracts the correlation ID from the context.
func ExtractCorrelationId(ctx context.Context) (string, error) {
	correlationId, ok := ctx.Value(CorrelationIdKey).(string)
	if !ok {
		return "", fmt.Errorf("correlation ID not found in context")
	}
	return correlationId, nil
}

// ExtractAggregateUUID extracts the aggregate UUID from the context.
func ExtractAggregateUUID(ctx context.Context) (string, error) {
	aggregateUUID, ok := ctx.Value(AggregateUUIDKey).(string)
	if !ok {
		return "", fmt.Errorf("aggregate UUID not found in context")
	}
	return aggregateUUID, nil
}

// ExtractApplication extracts the application name from the context.
func ExtractApplication(ctx context.Context) (string, error) {
	application, ok := ctx.Value(ApplicationKey).(string)
	if !ok {
		return "", fmt.Errorf("application not found in context")
	}
	return application, nil
}

// ExtractCommand extracts the command name from the context.
func ExtractCommand(ctx context.Context) (string, error) {
	command, ok := ctx.Value(CommandKey).(string)
	if !ok {
		return "", fmt.Errorf("command not found in context")
	}
	return command, nil
}

// ExtractHandler extracts the handler name from the context.
func ExtractHandler(ctx context.Context) (string, error) {
	handler, ok := ctx.Value(HandlerKey).(string)
	if !ok {
		return "", fmt.Errorf("handler not found in context")
	}
	return handler, nil
}

// MustExtractAggregateVersion extracts the aggregate version from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractAggregateVersion(ctx context.Context) string {
	if version, ok := ctx.Value(AggregateVersionKey).(string); ok {
		return version
	}
	return "0"
}

// MustExtractCausationId extracts the causation ID from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractCausationId(ctx context.Context) string {
	if causationId, ok := ctx.Value(CausationIdKey).(string); ok {
		return causationId
	}
	return ""
}

// MustExtractCorrelationId extracts the correlation ID from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractCorrelationId(ctx context.Context) string {
	if correlationId, ok := ctx.Value(CorrelationIdKey).(string); ok {
		return correlationId
	}
	return ""
}

// MustExtractAggregateID extracts the aggregate UUID from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractAggregateID(ctx context.Context) string {
	if aggregateUUID, ok := ctx.Value(AggregateUUIDKey).(string); ok {
		return aggregateUUID
	}
	return ""
}

// MustExtractApplication extracts the application name from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractApplication(ctx context.Context) string {
	if application, ok := ctx.Value(ApplicationKey).(string); ok {
		return application
	}
	return ""
}

// MustExtractCommand extracts the command name from the context.
// If the value is missing or of the wrong type, it returns an empty string.
func MustExtractCommand(ctx context.Context) string {
	if command, ok := ctx.Value(CommandKey).(string); ok {
		return command
	}
	return ""
}

// WithAggregateVersion adds the aggregate version to the context.
func WithAggregateVersion(ctx context.Context, version uint64) context.Context {
	return context.WithValue(ctx, AggregateVersionKey, fmt.Sprintf("%d", version))
}

// WithCausationId adds the causation ID to the context.
func WithCausationId(ctx context.Context, causationId string) context.Context {
	return context.WithValue(ctx, CausationIdKey, causationId)
}

// WithCorrelationId adds the correlation ID to the context.
func WithCorrelationId(ctx context.Context, correlationId string) context.Context {
	return context.WithValue(ctx, CorrelationIdKey, correlationId)
}

// WithAggregateID adds the aggregate UUID to the context.
func WithAggregateID(ctx context.Context, aggregateUUID string) context.Context {
	return context.WithValue(ctx, AggregateUUIDKey, aggregateUUID)
}

// WithApplication adds the application name to the context.
func WithApplication(ctx context.Context, application string) context.Context {
	return context.WithValue(ctx, ApplicationKey, application)
}

// WithCommand adds the command name to the context.
func WithCommand(ctx context.Context, command string) context.Context {
	return context.WithValue(ctx, CommandKey, command)
}

// WithHandler adds the handler name to the context.
func WithHandler(ctx context.Context, handler string) context.Context {
	return context.WithValue(ctx, HandlerKey, handler)
}
