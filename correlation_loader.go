package eventsourcing

import (
	"context"
	"errors"
)

// ErrEmptyCorrelationID is returned by [CorrelationLoader.LoadCorrelation]
// when it is asked for the chain of the empty correlation ID. Events saved
// without a CorrelationID share it, so it names no chain.
var ErrEmptyCorrelationID = errors.New("empty correlation ID")

// CorrelationLoader is implemented by event stores that can load a
// correlation chain: every event, across all streams, that shares one
// [Envelope.CorrelationID]. It is meant for debugging and auditing — to see
// everything a command set off — not for rebuilding state.
//
// CorrelationLoader is a separate interface from [EventStore] so that stores
// can adopt it independently, as with [GlobalReader].
//
// Every implementation is checked against the contract by
// eventsourcingtest.CorrelationLoaderAcceptanceTest.
type CorrelationLoader interface {
	// LoadCorrelation returns an iterator over every event whose
	// CorrelationID is id, in ascending GlobalPosition order. A chain spans
	// streams, so Version orders only the events within one of them.
	//
	// An id no event carries yields an empty iterator, not an error. The
	// empty id returns an error wrapping [ErrEmptyCorrelationID].
	//
	// The iterator follows the rules of the [EventStore] Load* methods: it is
	// bound to ctx, and a caller that may stop early must close it.
	LoadCorrelation(ctx context.Context, id string) (*Iterator[*Envelope], error)
}
