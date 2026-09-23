package eventsourcing

import (
	"context"
	"errors"
	"io"
	"iter"
	"slices"
)

// IterFunc produces the next item of an [Iterator]. It returns:
//   - (item, nil) for each item, in order;
//   - (zero, [io.EOF]), wrapped or not, after the last item;
//   - (zero, err) for any other failure, which ends iteration.
//
// An item returned together with a non-nil error is ignored, so the last
// item must be returned with a nil error. A function that stops partway
// through reading an item must return [io.ErrUnexpectedEOF] or another
// error rather than io.EOF; otherwise the partial read ends iteration as if
// the items were exhausted.
//
// ctx is the context the Iterator was created with. A function that blocks
// must return promptly once ctx is done.
type IterFunc[T any] func(ctx context.Context) (T, error)

// Iterator is a pull-based iterator over items of type T, such as the events
// returned by the Load methods of an [EventStore].
//
// Call Next to advance and Value to read the current item. When Next returns
// false, call Err to tell a clean end (nil) from a failure:
//
//	it, err := store.Load(ctx, id)
//	if err != nil {
//		return err
//	}
//	defer it.Close()
//	for it.Next() {
//		apply(it.Value())
//	}
//	return it.Err()
//
// An Iterator may hold resources, such as a database cursor, until iteration
// ends. Reaching the end or failing releases them automatically. A caller
// that may stop early must call Close; deferring Close is always safe.
// Alternatively, range over [Iterator.Values] or call [Iterator.All], both
// of which close the iterator.
//
// An Iterator is bound to the context it was created with. That context
// controls the entire iteration, not only the call that created it: keep it
// alive until iteration ends. Cancelling it ends iteration with the
// context's error.
//
// An Iterator is single-use and is not safe for concurrent use by multiple
// goroutines. Create one with [NewIteratorFunc] or [NewSliceIterator]; the
// zero Iterator is not usable and advancing it panics.
type Iterator[T any] struct {
	ctx   context.Context
	next  IterFunc[T]
	close func() error

	current  T
	err      error
	closeErr error
	done     bool
}

// NewIteratorFunc returns an Iterator bound to ctx that draws its items from
// next. ctx must be non-nil; NewIteratorFunc panics if it is nil, since the
// context cannot be supplied later.
//
// close releases the resources behind next, or is nil if there are none. The
// Iterator calls it exactly once: when Next first returns false, or when
// Close is called, whichever comes first. The close function
//   - must release every resource before returning, including stopping and
//     waiting for any goroutine next depends on;
//   - must not depend on ctx, which is often already cancelled when close
//     runs; derive a context with [context.WithoutCancel] and a timeout if
//     cleanup needs one;
//   - must refer to the resource that is current when it runs. A method value
//     such as rows.Close binds its receiver when it is evaluated, so use a
//     closure if the variable holding the resource is reassigned;
//   - must not panic. If it does, the panic propagates and close is not
//     called again, as with [sync.Once]. A caller that recovers from such a
//     panic must not read a nil Err as a successful iteration: the iterator
//     has no error to report because close never returned one.
func NewIteratorFunc[T any](ctx context.Context, next IterFunc[T], close func() error) *Iterator[T] {
	if ctx == nil {
		panic("eventsourcing: nil Context")
	}
	return &Iterator[T]{ctx: ctx, next: next, close: close}
}

// NewSliceIterator returns an Iterator bound to ctx that yields the elements
// of s in order. It holds no resources.
func NewSliceIterator[T any](ctx context.Context, s []T) *Iterator[T] {
	i := 0
	return NewIteratorFunc(ctx, func(context.Context) (T, error) {
		if i >= len(s) {
			var zero T
			return zero, io.EOF
		}
		v := s[i]
		i++
		return v, nil
	}, nil)
}

// Next advances the iterator to the next item, which is then available
// through Value. It returns false when the items are exhausted, when the
// iterator's IterFunc fails, when the iterator's context is done, or after
// Close. Before returning false for the first time, Next closes the
// iterator, so that call includes the cost of releasing its resources.
func (it *Iterator[T]) Next() bool {
	if it.done {
		return false
	}
	if it.ctx == nil {
		panic("eventsourcing: uninitialized Iterator; use NewIteratorFunc or NewSliceIterator")
	}
	if err := it.ctx.Err(); err != nil {
		it.finish(err)
		return false
	}
	v, err := it.next(it.ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = nil
		}
		it.finish(err)
		return false
	}
	it.current = v
	return true
}

func (it *Iterator[T]) finish(err error) {
	it.err = err
	_ = it.Close()
}

// Value returns the item produced by the most recent call to Next. It
// returns the zero value before the first call to Next and once Next has
// returned false. Items returned by earlier calls remain valid.
func (it *Iterator[T]) Value() T { return it.current }

// Err returns the error, if any, that ended iteration, joined with any error
// returned while closing the iterator. It returns nil if iteration ended
// because the items were exhausted. Err may be called after an explicit or
// implicit Close.
//
// Err reports failures, not completeness: after a caller closes the iterator
// early, Err returns nil unless closing failed. Code that requires a whole
// stream must therefore read to exhaustion or use All; an early Close
// followed by an Err check cannot tell a complete stream from a truncated
// one.
func (it *Iterator[T]) Err() error {
	switch {
	case it.closeErr == nil:
		return it.err
	case it.err == nil:
		return it.closeErr
	default:
		return errors.Join(it.err, it.closeErr)
	}
}

// Close releases the iterator's resources and ends iteration. It may be
// called more than once, after iteration has ended, and on a nil Iterator;
// every call returns the result of the first. After Close, Next returns
// false.
//
// Close on a nil Iterator returns nil rather than an error, unlike the
// nil-receiver checks in [os.File], so that a deferred Close is harmless
// after a Load that returned no iterator.
//
// Close must not be used to interrupt a Next running in another goroutine;
// cancel the iterator's context instead.
func (it *Iterator[T]) Close() error {
	if it == nil {
		return nil
	}
	if it.done {
		return it.closeErr
	}
	it.done = true
	var zero T
	it.current = zero
	if it.close != nil {
		it.closeErr = it.close()
	}
	return it.closeErr
}

// Wrap returns an iterator that draws its items from next, which reads from
// src. The returned iterator is bound to src's context, and closing it
// closes src exactly once, on every exit path.
//
// next must return [io.EOF] when src ends, whatever the reason. It must not
// return src.Err(): the source's failure, and any error from closing it,
// reach the returned iterator through Close, and returning them from next as
// well would report the same failure twice. next may call src.Next any
// number of times per item, so it can drop items or buffer several.
//
// done, if non-nil, is called exactly once when the returned iterator
// closes, with the error src ended with, or nil if it ended cleanly. It runs
// after src has been closed, so work ended there — a span, a metric, a
// leak check — covers the whole iteration.
func Wrap[T, U any](src *Iterator[T], next func(context.Context, *Iterator[T]) (U, error), done func(error)) *Iterator[U] {
	return NewIteratorFunc(src.ctx,
		func(ctx context.Context) (U, error) { return next(ctx, src) },
		func() error {
			_ = src.Close() // idempotent; its error is part of src.Err()
			err := src.Err()
			if done != nil {
				done(err)
			}
			return err
		})
}

// All reads the remaining items, closes the iterator, and returns the items
// along with the result of Err. On failure it returns the items read before
// the error, as [io.ReadAll] does.
//
// All returns only items not yet consumed by Next. On an iterator that is
// already exhausted or closed it returns no items, which cannot be
// distinguished from an empty sequence.
func (it *Iterator[T]) All() ([]T, error) {
	items := slices.Collect(it.Values())
	return items, it.Err()
}

// Values returns the remaining items as a sequence for use with range. The iterator is closed when the loop ends
// for any reason, including break, return, and panic. Errors are left to
// Err, as with a Next loop, so one rule covers both styles:
//
//	for e := range it.Values() {
//		apply(e)
//	}
//	return it.Err()
//
// Values returns a single-use sequence: like Next, it continues from the
// current position, does not restart iteration, and yields nothing on a
// closed iterator.
func (it *Iterator[T]) Values() iter.Seq[T] {
	return func(yield func(T) bool) {
		defer it.Close()
		for it.Next() {
			if !yield(it.Value()) {
				return
			}
		}
	}
}
