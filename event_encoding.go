package eventsourcing

import (
	"encoding/json"
	"fmt"
	"slices"
)

// EncodeEvent returns the encoding of event that a store persists under the
// name event.EventType(), and that [DecodeEvent] turns back into an event.
// Every store in this module encodes through it, so a store built on it
// writes the same bytes they do.
//
// It returns an error wrapping [ErrEventNotRegistered] unless event's concrete
// type is registered under event.EventType(), so a store calling it rejects
// an event at Save that it could not read back later.
func EncodeEvent(event Event) ([]byte, error) {
	if event == nil {
		return nil, fmt.Errorf("encode event: nil event")
	}
	name := event.EventType()
	if !slices.Contains(EventNamesFor(event), name) {
		return nil, fmt.Errorf("encode event %q: %w: %s", name, ErrEventNotRegistered, eventTypeKey(event))
	}
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("encode event %q: %w", name, err)
	}
	return data, nil
}

// DecodeEvent returns a new instance of the event registered under name,
// filled from data as [EncodeEvent] wrote it. It returns an error wrapping
// [ErrEventNotRegistered] if no event is registered under name.
func DecodeEvent(name string, data []byte) (Event, error) {
	ev, err := NewEventByName(name)
	if err != nil {
		return nil, fmt.Errorf("decode event %q: %w", name, err)
	}
	if err := json.Unmarshal(data, ev); err != nil {
		return nil, fmt.Errorf("decode event %q: %w", name, err)
	}
	return ev, nil
}

// EncodeMetadata returns the encoding of an [Envelope]'s Metadata that
// [DecodeMetadata] reads back. A nil map encodes as an empty one.
func EncodeMetadata(metadata map[string]any) ([]byte, error) {
	if metadata == nil {
		metadata = map[string]any{}
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode metadata: %w", err)
	}
	return data, nil
}

// DecodeMetadata returns the metadata [EncodeMetadata] encoded as data. Empty
// data, and data holding an encoded nil map, decode as an empty, non-nil map.
func DecodeMetadata(data []byte) (map[string]any, error) {
	var metadata map[string]any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("decode metadata: %w", err)
		}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return metadata, nil
}
