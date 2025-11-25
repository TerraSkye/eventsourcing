package kurrentdb

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	cqrs "github.com/terraskye/eventsourcing"
)

type eventstore struct {
	client *kurrentdb.Client
}

// NewEventStore creates a KurrentDB-backed eventstore
func NewEventStore(db *kurrentdb.Client) cqrs.EventStore {
	return &eventstore{
		client: db,
	}
}

func (e eventstore) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.Revision) (cqrs.AppendResult, error) {

	var kevents = make([]kurrentdb.EventData, len(events))

	for i, ev := range events {
		eventData, err := json.Marshal(ev.Event)

		if err != nil {
			return cqrs.AppendResult{Successful: false}, err
		}

		metaData, err := json.Marshal(ev.Metadata)

		if err != nil {
			return cqrs.AppendResult{Successful: false}, err
		}

		kevents[i] = kurrentdb.EventData{
			EventID:     ev.EventID,
			EventType:   cqrs.TypeName(ev.Event),
			ContentType: kurrentdb.ContentTypeJson,
			Data:        eventData,
			Metadata:    metaData,
		}
	}

	//todo use the revision here
	result, err := e.client.AppendToStream(ctx, events[0].StreamID, kurrentdb.AppendToStreamOptions{
		StreamState: kurrentdb.Any{},
	}, kevents...)

	if err != nil {
		return cqrs.AppendResult{Successful: false}, err
	}

	return cqrs.AppendResult{
		Successful:          true,
		NextExpectedVersion: result.NextExpectedVersion,
	}, nil

}

func (e eventstore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	streamer, err := e.client.ReadStream(ctx, id, kurrentdb.ReadStreamOptions{
		Direction: kurrentdb.Forwards,
		From: kurrentdb.StreamRevision{
			Value: 0,
		}, ResolveLinkTos: true,
	}, -1)

	if err != nil {
		return nil, cqrs.WrapEventStoreError(err)
	}

	iter := cqrs.NewIterator(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, cqrs.WrapEventStoreError(err)
		}

		// Convert KurrentDB event to cqrs.Event
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err))
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err))
		}

		var metadata map[string]any
		if err := json.Unmarshal(kEvent.Event.UserMetadata, &metadata); err != nil {
			metadata = make(map[string]any) // fallback to empty map
		}

		envelope := &cqrs.Envelope{
			EventID:    kEvent.Event.EventID,
			StreamID:   kEvent.Event.StreamID,
			Event:      ev,
			Metadata:   metadata,
			Version:    kEvent.Event.Position.Commit,
			OccurredAt: kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

func (e eventstore) LoadStreamFrom(ctx context.Context, id string, version uint64) (*cqrs.Iterator[*cqrs.Envelope], error) {

	streamer, err := e.client.ReadStream(ctx, id, kurrentdb.ReadStreamOptions{
		Direction: kurrentdb.Forwards,
		From: kurrentdb.StreamRevision{
			Value: version,
		}, ResolveLinkTos: true,
	}, -1)

	if err != nil {
		return nil, cqrs.WrapEventStoreError(err)
	}

	iter := cqrs.NewIterator(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, cqrs.WrapEventStoreError(err)
		}

		// Convert KurrentDB event to cqrs.Event
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err))
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err))
		}

		var metadata map[string]any
		if err := json.Unmarshal(kEvent.Event.UserMetadata, &metadata); err != nil {
			metadata = make(map[string]any) // fallback to empty map
		}

		envelope := &cqrs.Envelope{
			EventID:    kEvent.Event.EventID,
			StreamID:   kEvent.Event.StreamID,
			Event:      ev,
			Metadata:   metadata,
			Version:    kEvent.Event.Position.Commit,
			OccurredAt: kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

func (e eventstore) LoadFromAll(ctx context.Context, version uint64) (*cqrs.Iterator[*cqrs.Envelope], error) {

	streamer, err := e.client.ReadAll(ctx, kurrentdb.ReadAllOptions{
		Direction: kurrentdb.Forwards,
		From: kurrentdb.Position{
			Commit: version,
		},
		ResolveLinkTos: true,
	}, -1)

	if err != nil {
		return nil, cqrs.WrapEventStoreError(err)
	}

	iter := cqrs.NewIterator(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, cqrs.WrapEventStoreError(err)
		}

		// Convert KurrentDB event to cqrs.Event
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err))
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err))
		}

		var metadata map[string]any
		if err := json.Unmarshal(kEvent.Event.UserMetadata, &metadata); err != nil {
			metadata = make(map[string]any) // fallback to empty map
		}

		envelope := &cqrs.Envelope{
			EventID:    kEvent.Event.EventID,
			StreamID:   kEvent.Event.StreamID,
			Event:      ev,
			Metadata:   metadata,
			Version:    kEvent.Event.Position.Commit,
			OccurredAt: kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

func (e eventstore) Close() error {
	return e.client.Close()
}
