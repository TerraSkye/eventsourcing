package kurrentdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	"github.com/terraskye/eventsourcing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	_ eventsourcing.EventStore   = (*EventStore)(nil)
	_ eventsourcing.GlobalReader = (*EventStore)(nil)
)

// EventStore is a KurrentDB-backed [eventsourcing.EventStore] that also
// implements [eventsourcing.GlobalReader], delegating stream storage,
// ordering, and revision checks to the KurrentDB server itself. Create one
// with [NewEventStore].
type EventStore struct {
	client     *kurrentdb.Client
	newBackoff func() backoff.BackOff
}

// NewEventStore returns a KurrentDB-backed [EventStore] that uses db for all
// operations.
func NewEventStore(db *kurrentdb.Client, opts ...Option) *EventStore {
	e := &EventStore{
		client:     db,
		newBackoff: defaultSaveBackoff,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Option configures an [EventStore] returned by [NewEventStore].
type Option func(*EventStore)

// WithBackoff overrides the [backoff.BackOff] Save uses to retry transient
// gRPC errors, in place of the default described on Save's doc comment.
// newBackoff is called once per Save call, so it must return a fresh,
// zero-state backoff.BackOff each time rather than a shared, already-used
// instance.
func WithBackoff(newBackoff func() backoff.BackOff) Option {
	return func(e *EventStore) {
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
// the equivalent KurrentDB stream-state expectation: [eventsourcing.Any] skips the
// check, [eventsourcing.NoStream] requires the stream not to exist yet,
// [eventsourcing.StreamExists] requires that it already does, and a [eventsourcing.Revision]
// of N requires the stream to hold exactly N events. All events must
// have the same StreamID, or Save returns a non-nil error without contacting
// the server. The append is retried with backoff on transient gRPC errors
// (such as those from an in-progress leader election), for up to 30 seconds
// by default — see [WithBackoff] to override. On success it returns a [eventsourcing.AppendResult] whose
// NextExpectedVersion is the number of events the stream now holds.
//
// Failures are reported in this package's error vocabulary: a violated
// [eventsourcing.NoStream] expectation matches [eventsourcing.ErrStreamExists], a violated
// [eventsourcing.StreamExists] expectation matches [eventsourcing.ErrStreamNotFound], a
// violated [eventsourcing.Revision] is a [eventsourcing.StreamRevisionConflictError], and
// anything else is mapped by [mapError].
func (e EventStore) Save(ctx context.Context, events []eventsourcing.Envelope, revision eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
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
		if env.EventID == uuid.Nil {
			return eventsourcing.AppendResult{StreamID: streamID}, fmt.Errorf(
				"save events to stream %q: %w: event %d has no event ID",
				streamID, eventsourcing.ErrInvalidEventBatch, i,
			)
		}
	}

	var kEvents = make([]kurrentdb.EventData, len(events))

	for i, ev := range events {
		eventData, err := eventsourcing.EncodeEvent(ev.Event)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: event %d: %w", streamID, i, err)
		}

		// KurrentDB keeps the chain IDs in the metadata, under its own keys.
		// Those keys come only from the Envelope's fields: the same keys in
		// Metadata would be read back as the chain.
		metadata := make(map[string]any, len(ev.Metadata)+2)
		maps.Copy(metadata, ev.Metadata)
		delete(metadata, correlationIDKey)
		delete(metadata, causationIDKey)
		if ev.CorrelationID != "" {
			metadata[correlationIDKey] = ev.CorrelationID
		}
		if ev.CausationID != "" {
			metadata[causationIDKey] = ev.CausationID
		}
		metadataJSON, err := eventsourcing.EncodeMetadata(metadata)
		if err != nil {
			return eventsourcing.AppendResult{Successful: false, StreamID: streamID},
				fmt.Errorf("save events to stream %q: event %d: %w", streamID, i, err)
		}

		kEvents[i] = kurrentdb.EventData{
			EventID:     ev.EventID,
			EventType:   ev.Event.EventType(),
			ContentType: kurrentdb.ContentTypeJson,
			Data:        eventData,
			Metadata:    metadataJSON,
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
		GlobalPosition:      result.CommitPosition,
	}, nil

}

// currentRevision returns the number of events stream streamID holds, as a
// [eventsourcing.Revision], by reading its last event. It is only called
// after a failed append, to fill in a conflict's ActualRevision, so the
// count may already be newer than the one the append was rejected against.
// It returns nil if the read fails; [eventsourcing.StreamRevisionConflictError]
// renders a nil revision without trouble.
func (e EventStore) currentRevision(ctx context.Context, streamID string) eventsourcing.StreamState {
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
// ends with an error matching [eventsourcing.ErrStreamNotFound], as it does in the
// memory and file implementations. Because the read is lazy, that error
// arrives through the iterator's Err rather than from LoadStream itself —
// the server only reports the missing stream once the first event is
// requested. Every other failure reaches Err too, mapped through
// [mapError]; only a genuine end of stream ends iteration with a nil Err.
func (e EventStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return e.LoadStreamFrom(ctx, id, eventsourcing.StreamExists{})
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting after the position identified by version. A
// [eventsourcing.Revision] of N skips the stream's first N events, so a caller that
// has already consumed N events resumes exactly where it left off;
// [eventsourcing.Any] reads from the beginning. [eventsourcing.StreamExists] also reads from
// the beginning but requires the stream to exist, and [eventsourcing.NoStream]
// requires that it does not.
//
// Because the read is lazy, a violated precondition is reported through the
// iterator's Err rather than by LoadStreamFrom itself: a missing stream
// under StreamExists matches [eventsourcing.ErrStreamNotFound], and an existing one
// under NoStream matches [eventsourcing.ErrStreamExists]. Under Any or a Revision a
// missing stream is an empty read, which is what lets a command handler
// load a brand-new aggregate. Every other failure ends iteration with an
// error, mapped through [mapError].
func (e EventStore) LoadStreamFrom(ctx context.Context, id string, version eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
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

// scanLimit caps how many $all records one ReadAll call looks at when a
// filter matches rarely. The KurrentDB client cannot filter a plain read
// server-side, so filtering happens here, and without a cap a selective
// filter would scan the whole log in one call. Hitting the cap returns a
// page that may be empty but has moved Next forward, as the
// [eventsourcing.GlobalReader] contract allows.
const scanLimit = 4096

// ReadAll implements [eventsourcing.GlobalReader] over $all. GlobalPosition
// is an event's commit position.
//
// KurrentDB's own records in $all — events in system streams, and event
// types starting with "$", such as $metadata and link events — are skipped:
// they are not application events, and no application registers them.
// Links are not resolved either, so an event reached through a link is not
// returned a second time. Skipped records still move Next forward.
//
// KurrentDB only makes a position readable once everything before it is,
// so reading $all is gap-free without further work.
func (e EventStore) ReadAll(ctx context.Context, after uint64, opts eventsourcing.ReadOptions) (eventsourcing.ReadResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 500
	}
	types := make(map[string]bool, len(opts.EventTypes))
	for _, t := range opts.EventTypes {
		types[t] = true
	}

	var from kurrentdb.AllPosition = kurrentdb.Start{}
	if after > 0 {
		// KurrentDB includes the record at the position it is asked to read
		// from; records up to after are skipped below.
		from = kurrentdb.Position{Commit: after, Prepare: after}
	}

	// Read without a count (see readToEnd) and stop by closing the stream,
	// so that the end of the read is always the end of $all and AtEnd can
	// be told apart from running out of budget.
	scan := max(limit, scanLimit)
	streamer, err := e.client.ReadAll(ctx, kurrentdb.ReadAllOptions{
		Direction: kurrentdb.Forwards,
		From:      from,
	}, readToEnd)
	if err != nil {
		return eventsourcing.ReadResult{}, mapError("read all", err)
	}
	defer streamer.Close()

	res := eventsourcing.ReadResult{Next: after}
	scanned := 0
	for {
		kEvent, err := streamer.Recv()
		if errors.Is(err, io.EOF) {
			res.AtEnd = true
			return res, nil
		}
		if err != nil {
			return eventsourcing.ReadResult{}, mapError("read all", err)
		}
		rec := kEvent.Event
		if rec.Position.Commit <= after {
			continue
		}
		scanned++
		if scanned > scan || len(res.Events) == limit {
			// There is more after what this page covers.
			return res, nil
		}
		res.Next = rec.Position.Commit
		if isSystemRecord(rec) {
			continue
		}
		if len(types) > 0 && !types[rec.EventType] {
			continue
		}
		if !hasPrefix(rec.StreamID, opts.StreamPrefixes) {
			continue
		}
		env, err := toEnvelope(rec)
		if err != nil {
			return eventsourcing.ReadResult{}, fmt.Errorf("read all: %w", err)
		}
		res.Events = append(res.Events, env)
	}
}

// Head implements [eventsourcing.GlobalReader]: the commit position of the
// newest application event in $all, skipping KurrentDB's own records the
// way ReadAll does, or 0 if there is none.
func (e EventStore) Head(ctx context.Context) (uint64, error) {
	streamer, err := e.client.ReadAll(ctx, kurrentdb.ReadAllOptions{
		Direction: kurrentdb.Backwards,
		From:      kurrentdb.End{},
	}, readToEnd)
	if err != nil {
		return 0, mapError("head", err)
	}
	defer streamer.Close()
	for {
		kEvent, err := streamer.Recv()
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		if err != nil {
			return 0, mapError("head", err)
		}
		if !isSystemRecord(kEvent.Event) {
			return kEvent.Event.Position.Commit, nil
		}
	}
}

func hasPrefix(streamID string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(streamID, p) {
			return true
		}
	}
	return false
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
	ev, err := eventsourcing.DecodeEvent(rec.EventType, rec.Data)
	if err != nil {
		return nil, err
	}

	metadata, err := eventsourcing.DecodeMetadata(rec.UserMetadata)
	if err != nil {
		metadata = make(map[string]any) // fallback to empty map
	}
	correlationID, _ := metadata[correlationIDKey].(string)
	causationID, _ := metadata[causationIDKey].(string)
	delete(metadata, correlationIDKey)
	delete(metadata, causationIDKey)

	return &eventsourcing.Envelope{
		EventID:        rec.EventID,
		StreamID:       rec.StreamID,
		Event:          ev,
		Metadata:       metadata,
		Version:        rec.EventNumber + 1,
		GlobalPosition: rec.Position.Commit,
		OccurredAt:     rec.CreatedDate,
		CorrelationID:  correlationID,
		CausationID:    causationID,
	}, nil
}

// KurrentDB's own metadata keys for an event's chain, which its tooling and
// its $by_correlation_id projection read.
const (
	correlationIDKey = "$correlationId"
	causationIDKey   = "$causationId"
)

// Close closes the underlying KurrentDB client connection.
func (e EventStore) Close() error {
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
