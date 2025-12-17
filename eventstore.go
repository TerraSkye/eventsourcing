package eventsourcing

import (
	"context"
)

// EventStore defines the contract for an append-only event store
// used in event-sourced systems. An EventStore persists events
// associated with a given aggregate ID in sequential order, allowing
// for full reconstruction of aggregate state at any point in time.
//
// Implementations must guarantee:
//   - Events for a given aggregate are stored in order.
//   - Concurrency control based on the aggregate's expected version.
//   - Iteration order from all Load* methods is deterministic (oldest → newest).
//
// The returned iter.Seq values are lazy iterators over the stored events.
// They should be consumed immediately; no assumptions should be made about
// reusability or thread-safety after iteration completes.
type EventStore interface {
	// Save appends all events in the given slice to the event stream for a specific aggregate.
	//
	// Parameters:
	//   - ctx: Request-scoped context for cancellation and tracing.
	//   - events: A slice of Envelope values to append. Each envelope should have
	//     the aggregate ID and version set consistently.
	//   - revision: The expected stream state or concurrency requirement. This can
	//     be one of:
	//       - Any: always append, do not check for conflicts.
	//       - NoStream: stream must not exist; fail if it does.
	//       - StreamExists: stream must exist; fail if it does not.
	//		 -
	//
	// Errors:
	//   - ErrConcurrency if the originalVersion does not match.
	//   - Any store-specific persistence error.
	Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error)

	// LoadStream loads all events for the given aggregate ID from version 0 onward.
	//
	// The returned iterator yields events in ascending version order.
	// Iteration stops if:
	//   - The iterator function returns false (consumer stops early).
	//   - The context is canceled.
	//
	// Returns:
	//   - iter.Seq[*Envelope]: Lazy iterator over events.
	//   - error: Non-nil if the store could not read events.
	LoadStream(ctx context.Context, id string) (*Iterator[*Envelope], error)

	// LoadStreamFrom loads all events for the given aggregate ID starting at the specified version.
	//
	// Parameters:
	//   - ctx: Request-scoped context for cancellation and tracing.
	//   - id: Aggregate identifier.
	//   - version: Zero-based version index from which to start iteration.
	//
	// Returns:
	//   - EnvelopeIterator: Lazy iterator over events from
	LoadStreamFrom(ctx context.Context, id string, version uint64) (*Iterator[*Envelope], error)

	// LoadFromAll loads all events from all aggregates starting at the specified version index.
	//
	// The definition of "version" here is store-specific; it may represent:
	//   - A global monotonically increasing sequence number across all aggregates.
	//   - A local version within each aggregate (in which case, events are yielded
	//     from each aggregate starting at that version).
	//
	// Events should be yielded in chronological order as stored by the backend.
	// Consumers should not assume global ordering unless explicitly documented by
	// the implementation.
	LoadFromAll(ctx context.Context, version uint64) (*Iterator[*Envelope], error)
	// Close releases any resources held by the EventStore, such as network
	// connections or file handles. After Close is called, the EventStore should
	// not be used.
	//
	// Implementations should make Close idempotent.
	Close() error
}

// AppendResult describes the outcome of an append operation.
type AppendResult struct {
	Successful          bool
	NextExpectedVersion uint64
}
