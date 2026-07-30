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

// eventstore is a KurrentDB-backed [cqrs.EventStore], delegating stream
// storage, ordering, and revision checks to the KurrentDB server itself.
type eventstore struct {
	client *kurrentdb.Client
}

// NewEventStore returns a KurrentDB-backed [cqrs.EventStore] that uses db for
// all operations.
func NewEventStore(db *kurrentdb.Client) cqrs.EventStore {
	return &eventstore{
		client: db,
	}
}

// Save appends events to the stream they share, translating revision into
// the equivalent KurrentDB stream-state expectation: [cqrs.Any] skips the
// check, [cqrs.NoStream] requires the stream not to exist yet,
// [cqrs.StreamExists] requires that it already does, and a [cqrs.Revision]
// requires the stream's current revision to match exactly. All events must
// have the same StreamID, or Save returns a non-nil error without contacting
// the server. The append is retried with backoff on transient gRPC errors
// (such as those from an in-progress leader election), for up to 30 seconds
// or 10 attempts. On success it returns a [cqrs.AppendResult] whose
// NextExpectedVersion is the stream's new revision as reported by KurrentDB.
//
// TODO: on a revision conflict, the returned cqrs.StreamRevisionConflictError
// only has its Stream field set — ExpectedRevision and ActualRevision are
// left as their zero value (a nil cqrs.StreamState). Calling Error() on that
// value panics with a nil pointer dereference (confirmed), because
// StreamRevisionConflictError.Error calls ToRawInt64() on both fields. Any
// caller that logs or formats this error today will crash instead of seeing
// a conflict message.
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
			// TODO: extract the actual revisions from conflictErr (the
			// commented-out fields below). As written, ExpectedRevision and
			// ActualRevision are left nil, and StreamRevisionConflictError.Error
			// panics when called on a nil cqrs.StreamState. See the Save doc
			// comment above.
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
		StreamID:            streamID,
		NextExpectedVersion: result.NextExpectedVersion,
	}, nil

}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended.
//
// TODO: any error opening the read (including a genuine connectivity
// failure) is currently reported as cqrs.ErrStreamNotFound, and any error
// received while iterating — including a mid-stream connection drop, not
// just a clean end of stream — is reported to the iterator as io.EOF, i.e.
// as if the read had completed successfully. A caller has no way to
// distinguish "the stream does not exist" or "the read failed partway
// through" from a normal, complete read.
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
			// TODO: this treats every error from Recv (including a real
			// connection failure, confirmed possible per ReadStream.Recv in
			// the kurrentdb client) as a normal end of stream, so genuine
			// failures are silently reported as success. See the doc comment
			// above.
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
			EventID:       kEvent.Event.EventID,
			StreamID:      kEvent.Event.StreamID,
			Event:         ev,
			Metadata:      metadata,
			Version:       kEvent.Event.EventNumber,
			GlobalVersion: kEvent.Event.Position.Commit,
			OccurredAt:    kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting at the position identified by version. Only a
// positive [cqrs.Revision] changes the starting point; version.ToRawInt64()
// <= 0 — which includes [cqrs.NoStream], [cqrs.StreamExists], [cqrs.Any],
// and a zero [cqrs.Revision] — all read from the beginning of the stream.
// Unlike the memory, file, and postgres implementations of this interface,
// cqrs.NoStream and cqrs.StreamExists are not enforced as existence
// preconditions here.
//
// TODO: as with LoadStream, every error from Recv while iterating —
// including a real connection failure — is reported to the iterator as
// io.EOF, i.e. a normal end of stream, so a caller cannot tell a genuine
// failure apart from a complete read.
func (e eventstore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	// cqrs.Revision(N) means "N events already consumed, resume strictly
	// after N" — the same "exclusive" contract eventstore/memory and
	// eventstore/file both implement — but kurrentdb.StreamRevision{Value: N}
	// is KurrentDB's native, inclusive-of-N read position. Add 1 to convert
	// between the two conventions; Revision(0) still falls through to
	// kurrentdb.Start{} below, which already means the same thing.
	var from kurrentdb.StreamPosition
	if version.ToRawInt64() > 0 {
		from = kurrentdb.StreamRevision{
			Value: uint64(version.ToRawInt64()) + 1,
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
			// TODO: see the LoadStreamFrom doc comment above — this masks
			// real Recv errors as a normal end of stream.
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
			EventID:       kEvent.Event.EventID,
			StreamID:      kEvent.Event.StreamID,
			Event:         ev,
			Metadata:      metadata,
			Version:       kEvent.Event.EventNumber,
			GlobalVersion: kEvent.Event.Position.Commit,
			OccurredAt:    kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

// LoadFromAll returns a lazy iterator over every event across all streams,
// in the global order KurrentDB stored them.
//
// TODO: version is accepted but never used — every call reads from the very
// beginning of the $all stream regardless of the position passed in, so
// there is currently no way to resume a previous LoadFromAll from where it
// left off (already flagged in the code below).
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
			// Propagate as-is: io.EOF signals a normal end of stream, any
			// other error is a genuine failure.
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
			EventID:       kEvent.Event.EventID,
			StreamID:      kEvent.Event.StreamID,
			Event:         ev,
			Metadata:      metadata,
			Version:       kEvent.Event.EventNumber,
			GlobalVersion: kEvent.Event.Position.Commit,
			OccurredAt:    kEvent.Event.CreatedDate,
		}

		return envelope, nil
	})

	return iter, nil
}

// Close closes the underlying KurrentDB client connection.
func (e eventstore) Close() error {
	return e.client.Close()
}

// isRetryableError reports whether err is a transient gRPC error worth
// retrying (as opposed to a permanent failure such as an invalid argument).
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
