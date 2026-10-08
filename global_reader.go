package eventsourcing

import "context"

// ReadOptions narrows a [GlobalReader.ReadAll] call.
//
// The zero value reads every event, up to an implementation-chosen page
// size.
type ReadOptions struct {
	// Limit is the maximum number of events ReadAll returns. Zero or a
	// negative value lets the implementation pick a page size.
	//
	// Limit bounds the events returned, not the events scanned: a store that
	// filters in Go may scan more rows than Limit to fill a page.
	Limit int

	// EventTypes, when not empty, restricts the result to events registered
	// under one of these names. Matching is exact, even on backends whose
	// native filter matches prefixes. [EventGroupProcessor.StreamFilter]
	// returns a suitable value.
	EventTypes []string

	// StreamPrefixes, when not empty, restricts the result to events whose
	// StreamID starts with one of these prefixes.
	//
	// When both EventTypes and StreamPrefixes are set, an event must match
	// both.
	StreamPrefixes []string
}

// ReadResult is one page of the global log, as returned by
// [GlobalReader.ReadAll].
type ReadResult struct {
	// Events holds the matching events, in ascending
	// [Envelope.GlobalVersion] order. It may be empty even when Next has
	// advanced, if every event scanned was filtered out.
	Events []*Envelope

	// Next is the GlobalVersion of the last event the read scanned, whether
	// it matched the filter or not. Pass it as after to the next ReadAll
	// call, and store it as a checkpoint once Events are processed.
	//
	// Next lets a filtered reader move its checkpoint past events it is not
	// interested in, so a projection that matches rarely does not re-scan the
	// same stretch of log after every restart. When nothing was scanned,
	// Next equals the after argument.
	Next uint64

	// AtEnd reports that the read reached the head of the log: at the moment
	// of the read, no event with a GlobalVersion greater than Next was
	// visible. It is a snapshot; new events may be appended right after.
	//
	// A reader that sees AtEnd is caught up and can wait for new events
	// instead of reading again immediately.
	AtEnd bool
}

// GlobalReader is implemented by event stores that can read their global
// log — every event across all streams — in order, a page at a time. It is
// the one thing a store needs to support projections; catching up,
// waiting for new events, checkpointing and batching are done by the
// projection package on top of it.
//
// GlobalReader is a separate interface from [EventStore] so that stores can
// adopt it independently, and so that code which only appends and loads
// streams does not depend on it.
//
// # Contract
//
// Every implementation must uphold the following, and is checked against
// it by eventsourcingtest.GlobalReaderAcceptanceTest:
//
//   - Order: events are returned in strictly ascending GlobalVersion order.
//   - No gaps: once ReadAll has returned an event with GlobalVersion n, no
//     event with a GlobalVersion of n or lower may become visible later.
//     Stores where positions can become visible out of order (a SQL
//     sequence assigned before commit, for example) must hide events until
//     every lower position is either committed or permanently abandoned.
//   - Progress: if AtEnd is false, Next is greater than after.
//   - Exclusive start: ReadAll(ctx, n, ...) never returns the event at
//     position n itself. GlobalVersion 0 is never assigned to an event, so
//     after == 0 reads from the beginning of the log.
//   - Filters: EventTypes and StreamPrefixes are always honored, on the
//     server when the backend can and in Go otherwise.
type GlobalReader interface {
	// ReadAll returns the next page of the global log after position after.
	//
	// It does not block waiting for new events: at the head of the log it
	// returns an empty page with AtEnd set. Use a [Notifier], or poll, to
	// learn about new events.
	ReadAll(ctx context.Context, after uint64, opts ReadOptions) (ReadResult, error)

	// Head returns the GlobalVersion of the newest event a ReadAll call
	// started now could return, ignoring filters, or 0 if the log is empty.
	//
	// It is used to measure how far a reader lags behind, and to record
	// where a projection's history ends when it is rebuilt. It should be
	// cheap; implementations may return a slightly stale value, but never
	// one beyond what ReadAll would return.
	Head(ctx context.Context) (uint64, error)
}

// Notifier is optionally implemented by a [GlobalReader] that can signal
// new appends, so a reader at the head of the log can wake up as soon as
// something is written instead of waiting for its next poll.
//
// Notifications are a latency optimization only. A reader must still poll
// occasionally, and must not rely on a notification for every append.
type Notifier interface {
	// Notify returns a channel that receives a value whenever events have
	// been appended, until ctx is done, at which point the channel is
	// closed.
	//
	// Notifications are coalesced: several appends in quick succession may
	// produce a single value. Sending never blocks the writer, so a slow
	// receiver misses intermediate notifications but never the fact that
	// something changed since it last received.
	Notify(ctx context.Context) (<-chan struct{}, error)
}
