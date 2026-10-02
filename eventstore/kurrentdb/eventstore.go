package kurrentdb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	"github.com/terraskye/eventsourcing"
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
func NewEventStore(db *kurrentdb.Client, opts ...Option) eventsourcing.EventStore {
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

// KurrentDB numbers a stream's events from 0 and expresses an append
// expectation as the number of the stream's last event, where this package
// numbers them from 1 and a [eventsourcing.Revision] is a count of events.
// The conversion happens at this boundary only, so every caller sees the
// same numbering it gets from the memory, file and postgres stores:
//
//	Envelope.Version        = EventNumber + 1
//	Save(Revision(0))       → kurrentdb.NoStream{}
//	Save(Revision(N))       → kurrentdb.Revision(N - 1)
//	LoadStreamFrom(Rev(N))  → read from KurrentDB revision N
//	NextExpectedVersion     = last EventNumber written + 1

// Save appends events to the stream they share, translating revision into
// the equivalent KurrentDB stream-state expectation: [cqrs.Any] skips the
// check, [cqrs.NoStream] requires the stream not to exist yet,
// [cqrs.StreamExists] requires that it already does, and a [cqrs.Revision]
// of N requires the stream to hold exactly N events. All events must
// have the same StreamID, or Save returns a non-nil error without contacting
// the server. The append is retried with backoff on transient gRPC errors
// (such as those from an in-progress leader election), for up to 30 seconds
// by default — see [WithBackoff] to override. On success it returns a [cqrs.AppendResult] whose
// NextExpectedVersion is the number of events the stream now holds.
//
// Failures are reported in this package's error vocabulary: a violated
// [cqrs.NoStream] expectation matches [cqrs.ErrStreamExists], a violated
// [cqrs.StreamExists] expectation matches [cqrs.ErrStreamNotFound], a
// violated [cqrs.Revision] is a [cqrs.StreamRevisionConflictError], and
// anything else is mapped by [mapError].
func (e eventstore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	if len(events) == 0 {
		return eventsourcing.AppendResult{Successful: true, NextExpectedVersion: 0}, nil
	}

	var streamID = events[0].StreamID

	// Validate all events are for same stream
	for i, env := range events {
		if env.StreamID != streamID {
			return eventsourcing.AppendResult{
					StreamID: streamID,
				}, fmt.Errorf(
					"save events to stream %q: %w: event %d has different stream ID %q",
					streamID, eventsourcing.ErrInvalidEventBatch, i, env.StreamID,
				)
		}
	}

	var kEvents = make([]kurrentdb.EventData, len(events))

	for i, ev := range events {
		eventData, err := json.Marshal(ev.Event)

		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d failed to marshal event data %s",
				streamID, err, i, ev.Event.EventType(),
			)
		}

		metaData, err := json.Marshal(ev.Metadata)

		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf(
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
	case eventsourcing.Any:
		streamState = kurrentdb.Any{}
	case eventsourcing.NoStream:
		streamState = kurrentdb.NoStream{}
	case eventsourcing.StreamExists:
		streamState = kurrentdb.StreamExists{}
	case eventsourcing.Revision:
		// A stream of N events has N-1 as its last KurrentDB revision, and
		// a stream of none has no revision at all.
		if n := uint64(rev.ToRawInt64()); n == 0 {
			streamState = kurrentdb.NoStream{}
		} else {
			streamState = kurrentdb.Revision(n - 1)
		}
	default:
		err := fmt.Errorf("unsupported revision type for stream %s :%w", streamID, eventsourcing.ErrInvalidRevision)
		return eventsourcing.AppendResult{Successful: false, StreamID: streamID}, err
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

	if err != nil {
		// A violated expectation is reported the way the other
		// implementations report it, so callers can match it without
		// knowing which store is underneath.
		if isWrongExpectedVersion(err) {
			if rev, ok := revision.(eventsourcing.Revision); ok {
				return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
					&eventsourcing.StreamRevisionConflictError{
						Stream:           streamID,
						ExpectedRevision: rev,
						ActualRevision:   e.currentRevision(ctx, streamID),
					}
			}
		}
		if expectation := expectationError(streamID, revision, err); expectation != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID}, expectation
		}

		return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
			mapError(fmt.Sprintf("save events to stream %q", streamID), err)
	}

	return eventsourcing.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: result.NextExpectedVersion + 1,
	}, nil

}

// currentRevision returns the number of events stream streamID holds, as a
// [eventsourcing.Revision], by reading its last event. It is only called
// after a failed append, to fill in a conflict's ActualRevision, so the
// count may already be newer than the one the append was rejected against.
// It returns nil if the read fails; [eventsourcing.StreamRevisionConflictError]
// renders a nil revision without trouble.
func (e eventstore) currentRevision(ctx context.Context, streamID string) eventsourcing.StreamState {
	streamer, err := e.client.ReadStream(ctx, streamID, kurrentdb.ReadStreamOptions{
		Direction: kurrentdb.Backwards,
		From:      kurrentdb.End{},
	}, 1)
	if err != nil {
		return nil
	}
	defer streamer.Close()

	kEvent, err := streamer.Recv()
	switch {
	case err == nil:
		return eventsourcing.Revision(kEvent.OriginalEvent().EventNumber + 1)
	case isNotFound(err):
		return eventsourcing.Revision(0)
	default:
		return nil
	}
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
func (e eventstore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return e.LoadStreamFrom(ctx, id, eventsourcing.StreamExists{})
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting after the position identified by version. A
// [cqrs.Revision] of N skips the stream's first N events, so a caller that
// has already consumed N events resumes exactly where it left off;
// [cqrs.Any] reads from the beginning. [cqrs.StreamExists] also reads from
// the beginning but requires the stream to exist, and [cqrs.NoStream]
// requires that it does not.
//
// Because the read is lazy, a violated precondition is reported through the
// iterator's Err rather than by LoadStreamFrom itself: a missing stream
// under StreamExists matches [cqrs.ErrStreamNotFound], and an existing one
// under NoStream matches [cqrs.ErrStreamExists]. Under Any or a Revision a
// missing stream is an empty read, which is what lets a command handler
// load a brand-new aggregate. Every other failure ends iteration with an
// error, mapped through [mapError].
func (e eventstore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	var (
		from           kurrentdb.StreamPosition = kurrentdb.Start{}
		missingIsEmpty                          = true
		mustNotExist                            = false
	)
	switch v := version.(type) {
	case eventsourcing.Any:
	case eventsourcing.NoStream:
		mustNotExist = true
	case eventsourcing.StreamExists:
		missingIsEmpty = false
	case eventsourcing.Revision:
		// Revision(N) means "N events already consumed". The next one is
		// the stream's (N+1)th event, which KurrentDB numbers N.
		if n := v.ToRawInt64(); n > 0 {
			from = kurrentdb.StreamRevision{Value: uint64(n)}
		}
	default:
		return nil, fmt.Errorf("load stream %q: unsupported stream state %T: %w", id, version, eventsourcing.ErrInvalidRevision)
	}

	op := fmt.Sprintf("load stream %q", id)

	streamer, err := e.client.ReadStream(ctx, id, kurrentdb.ReadStreamOptions{
		Direction:      kurrentdb.Forwards,
		From:           from,
		ResolveLinkTos: true,
	}, readToEnd)
	if err != nil {
		return nil, mapError(op, err)
	}

	iter := eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*eventsourcing.Envelope, error) {
		kEvent, err := streamer.Recv()
		if err != nil {
			// The client reports a missing stream from the first Recv
			// rather than from opening the read, which is why it is caught
			// here. mapError passes io.EOF through untouched, so a clean
			// end of stream still ends iteration successfully.
			if missingIsEmpty && isNotFound(err) {
				return nil, io.EOF
			}
			return nil, mapError(op, err)
		}
		if mustNotExist {
			return nil, fmt.Errorf("%s: expected empty stream: %w", op, eventsourcing.ErrStreamExists)
		}
		return toEnvelope(kEvent.Event)
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
// in the global order KurrentDB stored them, starting after the position
// identified by version. A [cqrs.Revision] is a GlobalVersion — the commit
// position of an event this store returned — and the read resumes strictly
// after that event; every other [cqrs.StreamState] reads from the
// beginning.
//
// KurrentDB's own records in $all — events in system streams, and event
// types starting with "$", such as $metadata and link events — are skipped:
// they are not application events, and no application registers them.
// Links are not resolved either, so an event reached through a link is not
// returned a second time.
func (e eventstore) LoadFromAll(ctx context.Context, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	var (
		from  kurrentdb.AllPosition = kurrentdb.Start{}
		after uint64
	)
	if rev, ok := version.(eventsourcing.Revision); ok {
		if after = uint64(rev.ToRawInt64()); after > 0 {
			// KurrentDB includes the event at the position it is asked to
			// read from; that event is skipped below.
			from = kurrentdb.Position{Commit: after, Prepare: after}
		}
	}

	streamer, err := e.client.ReadAll(ctx, kurrentdb.ReadAllOptions{
		Direction: kurrentdb.Forwards,
		From:      from,
	}, readToEnd)

	if err != nil {
		return nil, mapError("load from all", err)
	}

	iter := eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*eventsourcing.Envelope, error) {
		for {
			kEvent, err := streamer.Recv()
			if err != nil {
				// io.EOF signals a normal end of stream and passes through
				// mapError untouched; any other error is a genuine failure.
				return nil, mapError("load from all", err)
			}
			rec := kEvent.Event
			if isSystemRecord(rec) || (after > 0 && rec.Position.Commit <= after) {
				continue
			}
			return toEnvelope(rec)
		}
	}, func() error {
		// The iterator owns the read stream: closing it releases the
		// client's subscription, whether iteration ran to the end or the
		// caller stopped early.
		streamer.Close()
		return nil
	})

	return iter, nil
}

// isSystemRecord reports whether rec is one of KurrentDB's own records
// rather than an application event: anything in a system stream, or of a
// system event type such as $metadata or the $> of a link.
func isSystemRecord(rec *kurrentdb.RecordedEvent) bool {
	return strings.HasPrefix(rec.StreamID, "$") || strings.HasPrefix(rec.EventType, "$")
}

// toEnvelope decodes rec into an [eventsourcing.Envelope], converting
// KurrentDB's 0-based EventNumber into the 1-based Version the package
// uses.
func toEnvelope(rec *kurrentdb.RecordedEvent) (*eventsourcing.Envelope, error) {
	ev, err := eventsourcing.NewEventByName(rec.EventType)
	if err != nil {
		return nil, fmt.Errorf("cannot create event %q: %w", rec.EventType, err)
	}

	if err := json.Unmarshal(rec.Data, ev); err != nil {
		return nil, fmt.Errorf("cannot unmarshal event %q: %w", rec.EventType, err)
	}

	var metadata map[string]any
	if err := json.Unmarshal(rec.UserMetadata, &metadata); err != nil {
		metadata = make(map[string]any) // fallback to empty map
	}

	return &eventsourcing.Envelope{
		EventID:       rec.EventID,
		StreamID:      rec.StreamID,
		Event:         ev,
		Metadata:      metadata,
		Version:       rec.EventNumber + 1,
		GlobalVersion: rec.Position.Commit,
		OccurredAt:    rec.CreatedDate,
	}, nil
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
