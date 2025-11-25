package eventsourcing

import "context"

// EnvelopeIterator defines a generic iterator over Envelopes.
type EnvelopeIterator struct {
	nextFunc func(ctx context.Context) (*Envelope, error)
	current  *Envelope
	err      error
}

// Next advances the iterator. Returns false if the iterator is done or an error occurred.
func (it *EnvelopeIterator) Next(ctx context.Context) bool {
	if it.err != nil {
		return false
	}

	it.current, it.err = it.nextFunc(ctx)
	return it.current != nil && it.err == nil
}

// Value returns the current envelope.
func (it *EnvelopeIterator) Value() *Envelope {
	return it.current
}

// Err returns the last error encountered during iteration.
func (it *EnvelopeIterator) Err() error {
	return it.err
}

// All consumes the iterator and returns all items in a slice.
func (it *EnvelopeIterator) All(ctx context.Context) ([]*Envelope, error) {
	var results []*Envelope
	for it.Next(ctx) {
		results = append(results, it.Value())
	}
	return results, it.Err()
}

// NewIterator creates a new EnvelopeIterator from a function that produces
// the next envelope. The function should return (nil, nil) when the iterator
// is finished, or (nil, err) on error.
func NewIterator(nextFunc func(ctx context.Context) (*Envelope, error)) *EnvelopeIterator {
	return &EnvelopeIterator{
		nextFunc: nextFunc,
	}
}

// Iterator is the struct that keeps track of the all the information needed to iterate over all the items.
type Iterator[Entity any, Criteria any] struct {
}

func (i *Iterator[Entity, Criteria]) Next(ctx context.Context) bool {
	return false
}

func (i *Iterator[Entity, Criteria]) Value() *Entity {
	return nil
}

func (i *Iterator[Entity, Criteria]) Err() error {
	return nil
}

func (i *Iterator[Entity, Criteria]) All(ctx context.Context) ([]*Entity, error) {
	var results []*Entity
	for i.Next(ctx) {
		results = append(results, i.Value())
	}
	return results, i.Err()
}
