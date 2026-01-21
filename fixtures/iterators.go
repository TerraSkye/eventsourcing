package fixtures

import (
	"context"
	"io"

	es "github.com/terraskye/eventsourcing"
)

// EmptyIterator returns an iterator that yields no items.
func EmptyIterator() *es.Iterator[*es.Envelope] {
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		return nil, io.EOF
	})
}

// FailingIterator returns an iterator that fails with the given error.
func FailingIterator(err error) *es.Iterator[*es.Envelope] {
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		return nil, err
	})
}

// SingleEnvelopeIterator returns an iterator that yields a single envelope.
func SingleEnvelopeIterator(env *es.Envelope) *es.Iterator[*es.Envelope] {
	returned := false
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		if returned {
			return nil, io.EOF
		}
		returned = true
		return env, nil
	})
}

// EnvelopeIteratorFromEvents creates an iterator from events.
func EnvelopeIteratorFromEvents(events ...es.Event) *es.Iterator[*es.Envelope] {
	envelopes := EnvelopesFromEvents(events...)
	return SliceIterator(envelopes)
}

// FailAfterNIterator returns an iterator that yields n items, then fails.
func FailAfterNIterator(envelopes []*es.Envelope, n int, err error) *es.Iterator[*es.Envelope] {
	idx := 0
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		if idx >= n {
			return nil, err
		}
		if idx >= len(envelopes) {
			return nil, io.EOF
		}
		env := envelopes[idx]
		idx++
		return env, nil
	})
}

// ContextAwareIterator returns an iterator that respects context cancellation.
func ContextAwareIterator(envelopes []*es.Envelope) *es.Iterator[*es.Envelope] {
	idx := 0
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if idx >= len(envelopes) {
			return nil, io.EOF
		}
		env := envelopes[idx]
		idx++
		return env, nil
	})
}

// DelayedIterator wraps an iterator with a callback before each Next.
// Useful for testing timing-sensitive scenarios.
func DelayedIterator(envelopes []*es.Envelope, beforeNext func()) *es.Iterator[*es.Envelope] {
	idx := 0
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		if beforeNext != nil {
			beforeNext()
		}

		if idx >= len(envelopes) {
			return nil, io.EOF
		}
		env := envelopes[idx]
		idx++
		return env, nil
	})
}

// CountingIterator wraps an iterator and counts iterations.
type CountingIterator struct {
	inner *es.Iterator[*es.Envelope]
	Count int
}

// NewCountingIterator creates a CountingIterator.
func NewCountingIterator(envelopes []*es.Envelope) *CountingIterator {
	ci := &CountingIterator{}
	idx := 0
	ci.inner = es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		if idx >= len(envelopes) {
			return nil, io.EOF
		}
		ci.Count++
		env := envelopes[idx]
		idx++
		return env, nil
	})
	return ci
}

// Iterator returns the underlying iterator.
func (c *CountingIterator) Iterator() *es.Iterator[*es.Envelope] {
	return c.inner
}
