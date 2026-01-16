package kurrentdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	cqrs "github.com/terraskye/eventsourcing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

func (e eventstore) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.StreamState) (cqrs.AppendResult, error) {
	if len(events) == 0 {
		return cqrs.AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	var streamID = events[0].StreamID

	// Validate all events are for same stream
	for i, env := range events {
		if env.StreamID != streamID {
			return cqrs.AppendResult{
					StreamID: streamID,
				}, fmt.Errorf(
					"save events to stream %q: %w: event %d has different stream ID %q",
					streamID, cqrs.ErrInvalidEventBatch, i, env.StreamID,
				)
		}
	}

	var kEvents = make([]kurrentdb.EventData, len(events))

	for i, ev := range events {
		eventData, err := json.Marshal(ev.Event)

		if err != nil {
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d failed to marshal event data %s",
				streamID, err, i, ev.Event.EventType(),
			)
		}

		metaData, err := json.Marshal(ev.Metadata)

		if err != nil {
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d failed to marshal meta data %s",
				streamID, err, i, ev.Event.EventType(),
			)
		}

		kEvents[i] = kurrentdb.EventData{
			EventID:     ev.EventID,
			EventType:   ev.Event.EventType(),
			ContentType: kurrentdb.ContentTypeJson,
			Data:        eventData,
			Metadata:    metaData,
		}
	}

	var streamState kurrentdb.StreamState
	// Handle revision enforcement
	switch rev := revision.(type) {
	case cqrs.Any:
		streamState = kurrentdb.Any{}
	case cqrs.NoStream:
		streamState = kurrentdb.NoStream{}
	case cqrs.StreamExists:
		streamState = kurrentdb.StreamExists{}
	case cqrs.Revision:
		streamState = kurrentdb.Revision(uint64(rev.ToRawInt64()))
	default:
		err := fmt.Errorf("unsupported revision type for stream %s :%w", streamID, cqrs.ErrInvalidRevision)
		return cqrs.AppendResult{Successful: false, StreamID: streamID}, err
	}

	// Configure backoff with longer max elapsed time for leader elections
	expBackoff := backoff.NewExponentialBackOff()
	expBackoff.MaxElapsedTime = 30 * time.Second // Allow up to 30s for leader election
	expBackoff.InitialInterval = 100 * time.Millisecond
	expBackoff.MaxInterval = 2 * time.Second

	result, err := backoff.RetryNotifyWithData(func() (*kurrentdb.WriteResult, error) {
		result, err := e.client.AppendToStream(ctx, streamID, kurrentdb.AppendToStreamOptions{
			StreamState: streamState,
		}, kEvents...)

		if err != nil {
			// Check if this is a retryable error
			if isRetryableError(err) {
				// Return error without wrapping - this signals backoff to retry
				return nil, err
			}
			// Non-retryable error - mark as permanent
			return nil, backoff.Permanent(err)
		}
		return result, err

	}, backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 10), func(err error, duration time.Duration) {
		fmt.Printf("Retry attempt failed for stream %s: %v, retrying in %s", streamID, err, duration)
	})
	//todo use the revision here

	if err != nil {
		var conflictErr *kurrentdb.StreamRevisionConflictError
		if errors.As(err, &conflictErr) {
			//TODO extract the actual revisions..
			return cqrs.AppendResult{
					Successful: false,
					StreamID:   streamID,
				}, &cqrs.StreamRevisionConflictError{
					Stream: conflictErr.Stream,
					//ExpectedRevision: conflictErr.ExpectedRevision,
					//ActualRevision:   conflictErr.ActualRevision,
				}
		}

		// this is an unexpected error when saving.
		return cqrs.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf(
			"save events to stream %q: persist failed: %w",
			streamID, err,
		)
	}

	return cqrs.AppendResult{
		Successful:          true,
		NextExpectedVersion: result.NextExpectedVersion,
	}, nil

}

func (e eventstore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	streamer, err := e.client.ReadStream(ctx, id, kurrentdb.ReadStreamOptions{
		Direction:      kurrentdb.Forwards,
		From:           kurrentdb.Start{},
		ResolveLinkTos: true,
	}, 5000)

	if err != nil {
		//TODO enhance error variants
		return nil, fmt.Errorf(
			"load stream %q: failed to check existence: %w",
			id, cqrs.ErrStreamNotFound,
		)
	}

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf(
				"load stream %q: iteration failed : %w",
				id, ctx.Err(),
			)
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, io.EOF
		}

		// Convert KurrentDB event to cqrs.EventData
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err)
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err)
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

func (e eventstore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	var from kurrentdb.StreamPosition
	if version.ToRawInt64() > 0 {
		from = kurrentdb.StreamRevision{
			Value: uint64(version.ToRawInt64()),
		}
	} else {
		from = kurrentdb.Start{}
	}

	opt := kurrentdb.ReadStreamOptions{
		Direction:      kurrentdb.Forwards,
		From:           from,
		ResolveLinkTos: true,
	}
	streamer, err := e.client.ReadStream(ctx, id, opt, 5000)

	if err != nil {
		return nil, fmt.Errorf(
			"load stream %q: failed to check existence: %w",
			id, cqrs.ErrStreamNotFound,
		)
	}

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, io.EOF
		}

		// Convert KurrentDB event to cqrs.EventData
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err)
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err)
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

func (e eventstore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	//TODO fix `from`

	streamer, err := e.client.ReadAll(ctx, kurrentdb.ReadAllOptions{
		Direction:      kurrentdb.Forwards,
		From:           kurrentdb.Start{},
		ResolveLinkTos: true,
	}, 0)

	if err != nil {
		return nil, err
	}

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (*cqrs.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		kEvent, err := streamer.Recv()
		if err != nil {
			// Stream finished normally
			return nil, err
		}

		// Convert KurrentDB event to cqrs.EventData
		ev, err := cqrs.NewEventByName(kEvent.Event.EventType)
		if err != nil {
			// Wrap and propagate as EventStoreError
			return nil, fmt.Errorf("cannot create event %q: %w", kEvent.Event.EventType, err)
		}

		if err := json.Unmarshal(kEvent.Event.Data, ev); err != nil {
			return nil, fmt.Errorf("cannot unmarshal event %q: %w", kEvent.Event.EventType, err)
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

// isRetryableError determines if an error should trigger a retry
func isRetryableError(err error) bool {
	s, ok := status.FromError(err)
	if !ok {
		// Not a gRPC error - don't retry
		return false
	}

	// Retry on transient gRPC errors
	switch s.Code() {
	case codes.Unavailable: // Service unavailable (leader election, network issues)
		return true
	case codes.DeadlineExceeded: // Timeout
		return true
	case codes.ResourceExhausted: // Temporary overload
		return true
	case codes.Aborted: // Transaction aborted, might be retryable
		return true
	default:
		// Permanent errors like InvalidArgument, AlreadyExists, FailedPrecondition
		return false
	}
}
