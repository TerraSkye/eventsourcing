package kurrentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
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
	client     *kurrentdb.Client
	newBackoff func() backoff.BackOff
}

// NewEventStore returns a KurrentDB-backed [cqrs.EventStore] that uses db for
// all operations.
func NewEventStore(db *kurrentdb.Client, opts ...Option) cqrs.EventStore {
	e := &eventstore{
		client:     db,
		newBackoff: defaultSaveBackoff,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Option configures an [eventstore] returned by [NewEventStore].
type Option func(*eventstore)

// WithBackoff overrides the [backoff.BackOff] Save uses to retry transient
// gRPC errors, in place of the default described on Save's doc comment.
// newBackoff is called once per Save call, so it must return a fresh,
// zero-state backoff.BackOff each time rather than a shared, already-used
// instance.
func WithBackoff(newBackoff func() backoff.BackOff) Option {
	return func(e *eventstore) {
		e.newBackoff = newBackoff
	}
}

// defaultSaveBackoff is the backoff Save retries transient gRPC errors with
// unless overridden via [WithBackoff]: up to 30s, to allow for an
// in-progress leader election.
// readToEnd is the count passed to the KurrentDB client's read calls. That
// argument is not a page size the client transparently re-requests past: it
// is sent verbatim as ReadReq.Options.CountOption.Count, a server-enforced
// cap on the whole read, after which the server ends the gRPC stream and
// Recv reports a plain io.EOF — indistinguishable from a real end of
// stream. Only a count no stream can reach reads every event.
const readToEnd = math.MaxInt64

func defaultSaveBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = 30 * time.Second
	b.InitialInterval = 100 * time.Millisecond
	b.MaxInterval = 2 * time.Second
	return b
}

// Save appends events to the stream they share, translating revision into
// the equivalent KurrentDB stream-state expectation: [cqrs.Any] skips the
// check, [cqrs.NoStream] requires the stream not to exist yet,
// [cqrs.StreamExists] requires that it already does, and a [cqrs.Revision]
// requires the stream's current revision to match exactly. All events must
// have the same StreamID, or Save returns a non-nil error without contacting
// the server. The append is retried with backoff on transient gRPC errors
// (such as those from an in-progress leader election), for up to 30 seconds
// by default — see [WithBackoff] to override. On success it returns a [cqrs.AppendResult] whose
// NextExpectedVersion is the stream's new revision as reported by KurrentDB.
//
// Failures are reported in this package's error vocabulary: a violated
// [cqrs.NoStream] expectation matches [cqrs.ErrStreamExists], a violated
// [cqrs.StreamExists] expectation matches [cqrs.ErrStreamNotFound], and
// anything else is mapped by [mapError]. The KurrentDB error stays in the
// chain in every case.
//
// TODO: a violated [cqrs.Revision] expectation is still reported only as a
// wrapped client error, not as a [cqrs.StreamRevisionConflictError], so
// callers that retry on conflict — NewCommandHandler among them — do not
// recognise it. Translating it needs the stream's actual revision, which the
// client carries only for its StreamRevisionConflict code and not for the
// WrongExpectedVersion an ordinary append failure returns.
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

	expBackoff := e.newBackoff()

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

	}, expBackoff, func(err error, duration time.Duration) {
		fmt.Printf("Retry attempt failed for stream %s: %v, retrying in %s", streamID, err, duration)
	})
	//todo use the revision here

	if err != nil {
		// A violated NoStream or StreamExists precondition is reported with
		// the same sentinel the other implementations use, so callers can
		// match it without knowing which store is underneath.
		if expectation := expectationError(streamID, revision, err); expectation != nil {
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, expectation
		}

		return cqrs.AppendResult{Successful: false, StreamID: streamID},
			mapError(fmt.Sprintf("save events to stream %q", streamID), err)
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
// A stream that does not exist is a failure, not an empty read: iteration
// ends with an error matching [cqrs.ErrStreamNotFound], as it does in the
// memory and file implementations. Because the read is lazy, that error
// arrives through the iterator's Err rather than from LoadStream itself —
// the server only reports the missing stream once the first event is
// requested. Every other failure reaches Err too, mapped through
// [mapError]; only a genuine end of stream ends iteration with a nil Err.
func (e eventstore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	streamer, err := e.client.ReadStream(ctx, id, kurrentdb.ReadStreamOptions{
		Direction:      kurrentdb.Forwards,
		From:           kurrentdb.Start{},
		ResolveLinkTos: true,
	}, readToEnd)

	if err != nil {
		return nil, mapError(fmt.Sprintf("load stream %q", id), err)
	}

	iter := cqrs.NewIteratorFunc(ctx, func(context.Context) (*cqrs.Envelope, error) {
		kEvent, err := streamer.Recv()
		if err != nil {
			// mapError passes io.EOF through untouched, so a clean end of
			// stream still ends iteration successfully; everything else —
			// a missing stream, a dropped connection — surfaces through Err.
			return nil, mapError(fmt.Sprintf("load stream %q", id), err)
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
	}, func() error {
		// The iterator owns the read stream: closing it releases the
		// client's subscription, whether iteration ran to the end or the
		// caller stopped early.
		streamer.Close()
		return nil
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
// Consistent with not enforcing those preconditions, a stream that does not
// exist is an empty read rather than a failure — the same treatment Any{}
// gets in the memory and file implementations, and what lets a command
// handler load a brand-new aggregate. Every other failure ends iteration
// with an error, mapped through [mapError]; use [LoadStream] when a missing
// stream should be reported as [cqrs.ErrStreamNotFound].
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
	streamer, err := e.client.ReadStream(ctx, id, opt, readToEnd)

	if err != nil {
		return nil, mapError(fmt.Sprintf("load stream %q", id), err)
	}

	iter := cqrs.NewIteratorFunc(ctx, func(context.Context) (*cqrs.Envelope, error) {
		kEvent, err := streamer.Recv()
		if err != nil {
			// This method does not enforce existence preconditions, so an
			// absent stream is an empty read, not a failure. The client
			// reports it from the first Recv rather than from opening the
			// read, which is why it is caught here.
			if isNotFound(err) {
				return nil, io.EOF
			}
			return nil, mapError(fmt.Sprintf("load stream %q", id), err)
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
	}, func() error {
		// The iterator owns the read stream: closing it releases the
		// client's subscription, whether iteration ran to the end or the
		// caller stopped early.
		streamer.Close()
		return nil
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
	}, readToEnd)

	if err != nil {
		return nil, mapError("load from all", err)
	}

	iter := cqrs.NewIteratorFunc(ctx, func(context.Context) (*cqrs.Envelope, error) {
		kEvent, err := streamer.Recv()
		if err != nil {
			// io.EOF signals a normal end of stream and passes through
			// mapError untouched; any other error is a genuine failure.
			return nil, mapError("load from all", err)
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
	}, func() error {
		// The iterator owns the read stream: closing it releases the
		// client's subscription, whether iteration ran to the end or the
		// caller stopped early.
		streamer.Close()
		return nil
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
