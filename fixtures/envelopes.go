package fixtures

import (
	"time"

	"github.com/google/uuid"
	es "github.com/terraskye/eventsourcing"
)

// EnvelopeOption is a functional option for configuring an Envelope.
type EnvelopeOption func(*es.Envelope)

// NewEnvelope creates an Envelope with the given event and options.
func NewEnvelope(event es.Event, opts ...EnvelopeOption) *es.Envelope {
	env := &es.Envelope{
		EventID:       uuid.New(),
		StreamID:      event.AggregateID(),
		Event:         event,
		Version:       1,
		GlobalVersion: 1,
		OccurredAt:    time.Now(),
		Metadata:      make(map[string]any),
	}

	for _, opt := range opts {
		opt(env)
	}

	return env
}

// WithEventID sets a specific event ID.
func WithEventID(id uuid.UUID) EnvelopeOption {
	return func(e *es.Envelope) {
		e.EventID = id
	}
}

// WithStreamID overrides the stream ID (defaults to event's AggregateID).
func WithStreamID(id string) EnvelopeOption {
	return func(e *es.Envelope) {
		e.StreamID = id
	}
}

// WithVersion sets the stream version.
func WithVersion(v uint64) EnvelopeOption {
	return func(e *es.Envelope) {
		e.Version = v
	}
}

// WithGlobalVersion sets the global version.
func WithGlobalVersion(v uint64) EnvelopeOption {
	return func(e *es.Envelope) {
		e.GlobalVersion = v
	}
}

// WithTimestamp sets the occurred-at timestamp.
func WithTimestamp(t time.Time) EnvelopeOption {
	return func(e *es.Envelope) {
		e.OccurredAt = t
	}
}

// WithMetadata sets the entire metadata map.
func WithMetadata(m map[string]any) EnvelopeOption {
	return func(e *es.Envelope) {
		e.Metadata = m
	}
}

// WithMetadataField adds a single metadata field.
func WithMetadataField(key string, value any) EnvelopeOption {
	return func(e *es.Envelope) {
		if e.Metadata == nil {
			e.Metadata = make(map[string]any)
		}
		e.Metadata[key] = value
	}
}

// EnvelopeBuilder provides a fluent API for constructing envelopes.
type EnvelopeBuilder struct {
	eventID       uuid.UUID
	streamID      string
	event         es.Event
	version       uint64
	globalVersion uint64
	occurredAt    time.Time
	metadata      map[string]any
}

// NewEnvelopeBuilder creates a new EnvelopeBuilder with defaults.
func NewEnvelopeBuilder() *EnvelopeBuilder {
	return &EnvelopeBuilder{
		eventID:       uuid.New(),
		streamID:      "stream-1",
		event:         TestEvent{ID: "stream-1", Type: "TestEvent"},
		version:       1,
		globalVersion: 1,
		occurredAt:    time.Now(),
		metadata:      make(map[string]any),
	}
}

// WithEventID sets a specific event ID.
func (b *EnvelopeBuilder) WithEventID(id uuid.UUID) *EnvelopeBuilder {
	b.eventID = id
	return b
}

// WithStreamID sets the stream ID.
func (b *EnvelopeBuilder) WithStreamID(id string) *EnvelopeBuilder {
	b.streamID = id
	return b
}

// WithEvent sets the event.
func (b *EnvelopeBuilder) WithEvent(e es.Event) *EnvelopeBuilder {
	b.event = e
	b.streamID = e.AggregateID()
	return b
}

// WithVersion sets the stream version.
func (b *EnvelopeBuilder) WithVersion(v uint64) *EnvelopeBuilder {
	b.version = v
	return b
}

// WithGlobalVersion sets the global version.
func (b *EnvelopeBuilder) WithGlobalVersion(v uint64) *EnvelopeBuilder {
	b.globalVersion = v
	return b
}

// WithTimestamp sets the occurred-at timestamp.
func (b *EnvelopeBuilder) WithTimestamp(t time.Time) *EnvelopeBuilder {
	b.occurredAt = t
	return b
}

// WithMetadata sets the entire metadata map.
func (b *EnvelopeBuilder) WithMetadata(m map[string]any) *EnvelopeBuilder {
	b.metadata = m
	return b
}

// WithMetadataField adds a single metadata field.
func (b *EnvelopeBuilder) WithMetadataField(key string, value any) *EnvelopeBuilder {
	b.metadata[key] = value
	return b
}

// Build constructs the Envelope.
func (b *EnvelopeBuilder) Build() *es.Envelope {
	return &es.Envelope{
		EventID:       b.eventID,
		StreamID:      b.streamID,
		Event:         b.event,
		Version:       b.version,
		GlobalVersion: b.globalVersion,
		OccurredAt:    b.occurredAt,
		Metadata:      b.metadata,
	}
}

// BuildValue returns the envelope as a value (not pointer).
func (b *EnvelopeBuilder) BuildValue() es.Envelope {
	return *b.Build()
}

// EnvelopesFromEvents creates envelopes from a slice of events with sequential versions.
func EnvelopesFromEvents(events ...es.Event) []*es.Envelope {
	envelopes := make([]*es.Envelope, len(events))
	baseTime := time.Now()

	for i, event := range events {
		envelopes[i] = &es.Envelope{
			EventID:       uuid.New(),
			StreamID:      event.AggregateID(),
			Event:         event,
			Version:       uint64(i + 1),
			GlobalVersion: uint64(i + 1),
			OccurredAt:    baseTime.Add(time.Duration(i) * time.Millisecond),
			Metadata:      make(map[string]any),
		}
	}

	return envelopes
}

// EnvelopeValuesFromEvents creates envelope values from a slice of events.
func EnvelopeValuesFromEvents(events ...es.Event) []es.Envelope {
	ptrs := EnvelopesFromEvents(events...)
	values := make([]es.Envelope, len(ptrs))
	for i, p := range ptrs {
		values[i] = *p
	}
	return values
}
