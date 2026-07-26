package eventsourcing

import (
	"context"
)

// EventStore is an append-only store of [Envelope]s, grouped into per-
// aggregate streams, that allows an aggregate's state to be reconstructed by
// replaying its stream.
//
// Implementations must store events for a given stream in the order they
// were saved and yield them in that same order — oldest first — from every
// Load* method. The [Iterator] values Load* methods return are lazy and
// should be consumed promptly; implementations make no guarantee about
// their reusability or thread-safety once iteration ends.
type EventStore interface {
	// Save appends events to the stream identified by their common StreamID,
	// which must be the same across every element of events. revision states
	// the caller's expectation of the stream's current state — [Any] to
	// append unconditionally, [NoStream] to require the stream not already
	// exist, [StreamExists] to require that it does, or a specific [Revision]
	// to require an exact version — and Save returns a
	// [StreamRevisionConflictError] if that expectation does not hold.
	Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error)

	// LoadStream returns an iterator over every event in id's stream, in
	// ascending version order.
	LoadStream(ctx context.Context, id string) (*Iterator[*Envelope], error)

	// LoadStreamFrom returns an iterator over id's stream starting at
	// version, in ascending version order. version is typically a
	// [Revision]; [Any] starts from the beginning of the stream.
	LoadStreamFrom(ctx context.Context, id string, version StreamState) (*Iterator[*Envelope], error)

	// LoadFromAll returns an iterator over events across every stream,
	// starting at version. Whether version and the iteration order it
	// produces are globally consistent, or only consistent within each
	// stream, is implementation-specific; consult the implementation before
	// relying on cross-stream ordering.
	LoadFromAll(ctx context.Context, version StreamState) (*Iterator[*Envelope], error)

	// Close releases any resources held by the EventStore, such as network
	// connections or file handles. Implementations should make Close
	// idempotent. The EventStore must not be used after Close is called.
	Close() error
}

// AppendResult describes the outcome of an [EventStore.Save] call.
type AppendResult struct {
	// Successful reports whether the events were persisted.
	Successful bool
	// StreamID is the stream the events were saved to.
	StreamID string
	// NextExpectedVersion is the version the stream's next [Revision] should
	// use.
	NextExpectedVersion uint64
}
