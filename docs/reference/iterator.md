# Iterator

```go
type Iterator[T any] struct { ... }
```

A generic, lazy, pull-based iterator over items of type `T`. Used by `EventStore` to stream events without loading them all into memory.

An `Iterator` may hold resources — a database cursor, a network stream — until iteration ends. Reaching the end or failing releases them automatically; a caller that may stop early must call `Close`.

An `Iterator` is bound to the context it was created with. That context controls the whole iteration, not just the call that created it: keep it alive until iteration ends. Cancelling it ends iteration with the context's error.

An `Iterator` is single-use and not safe for concurrent use by multiple goroutines.

## Methods

### Next

```go
func (it *Iterator[T]) Next() bool
```

Advances to the next item. Returns `true` if a value is available, `false` at end-of-stream, on error, when the iterator's context is done, or after `Close`. Before returning `false` for the first time, `Next` closes the iterator.

`Next` takes no context — the iterator already holds the one it was created with.

### Value

```go
func (it *Iterator[T]) Value() T
```

Returns the current item. Returns the zero value before the first `Next` and once `Next` has returned `false`.

### Err

```go
func (it *Iterator[T]) Err() error
```

Returns the error that ended iteration, joined with any error from closing. `nil` means the items were exhausted cleanly. Always check it after the loop.

`Err` reports failures, not completeness: after an early `Close`, `Err` returns `nil` unless closing itself failed. Code that needs the whole stream must read to exhaustion or use `All` — an early `Close` followed by an `Err` check cannot tell a complete stream from a truncated one.

### Close

```go
func (it *Iterator[T]) Close() error
```

Releases the iterator's resources and ends iteration. Safe to call more than once, after iteration has ended, and on a `nil` iterator; every call returns the result of the first. Deferring `Close` is always safe.

`Close` must not be used to interrupt a `Next` running in another goroutine — cancel the iterator's context instead.

### All

```go
func (it *Iterator[T]) All() ([]T, error)
```

Reads the remaining items, closes the iterator, and returns them along with the result of `Err`. On failure it returns the items read before the error, as `io.ReadAll` does.

`All` returns only items not yet consumed by `Next`. On an already exhausted or closed iterator it returns no items, which is indistinguishable from an empty sequence.

### Values

```go
func (it *Iterator[T]) Values() iter.Seq[T]
```

Returns the remaining items as a sequence for use with `range`. The iterator is closed when the loop ends for any reason, including `break`, `return`, and `panic`. Errors are left to `Err`, as in a `Next` loop.

Like `Next`, the sequence is single-use: it continues from the current position and yields nothing on a closed iterator.

---

## Usage patterns

A `Next` loop, with `Close` deferred so an early `return` still releases resources:

```go
iter, err := store.LoadStream(ctx, streamID)
if err != nil {
    return err
}
defer iter.Close()

for iter.Next() {
    envelope := iter.Value()
    state = evolve(state, envelope)
}
return iter.Err()
```

Or `range` over `Values`, which closes the iterator itself:

```go
for envelope := range iter.Values() {
    state = evolve(state, envelope)
}
return iter.Err()
```

Or collect everything, for small streams:

```go
envelopes, err := iter.All()
```

---

## Constructors

### NewIteratorFunc

```go
func NewIteratorFunc[T any](ctx context.Context, next IterFunc[T], close func() error) *Iterator[T]
```

Creates an `Iterator[T]` bound to `ctx` that draws its items from `next`. `ctx` must be non-nil; `NewIteratorFunc` panics if it is nil, since the context cannot be supplied later.

`next` must return `(T, nil)` for each item, `(zero, io.EOF)` — wrapped or not — after the last one, and `(zero, err)` for any other failure. An item returned together with a non-nil error is ignored, so the last item must be returned with a nil error. A `next` that stops partway through reading an item must return `io.ErrUnexpectedEOF` or another error rather than `io.EOF`, or the partial read ends iteration as if the items were exhausted.

`close` releases the resources behind `next`, or is `nil` if there are none. The iterator calls it exactly once: when `Next` first returns `false`, or when `Close` is called, whichever comes first. It must release every resource before returning (including stopping and waiting for any goroutine `next` depends on), and must not depend on `ctx`, which is often already cancelled when `close` runs.

```go
rows, err := db.Query(ctx, sql)
if err != nil {
    return nil, err
}

return eventsourcing.NewIteratorFunc(ctx, func(context.Context) (*Envelope, error) {
    if !rows.Next() {
        if err := rows.Err(); err != nil {
            return nil, err
        }
        return nil, io.EOF
    }
    return scan(rows)
}, func() error {
    rows.Close()
    return nil
}), nil
```

### NewSliceIterator

```go
func NewSliceIterator[T any](ctx context.Context, s []T) *Iterator[T]
```

Creates an `Iterator[T]` bound to `ctx` that yields the elements of `s` in order. It holds no resources. Useful for testing and in-memory implementations.

```go
iter := eventsourcing.NewSliceIterator(ctx, envelopes)
```

### Wrap

```go
func Wrap[T, U any](
    src *Iterator[T],
    next func(context.Context, *Iterator[T]) (U, error),
    done func(error),
) *Iterator[U]
```

Creates an iterator that draws its items from `next`, which reads from `src` — the way to decorate, filter, or map an existing iterator. The result is bound to `src`'s context, and closing it closes `src` exactly once, on every exit path.

`next` must return `io.EOF` when `src` ends, whatever the reason. It must **not** return `src.Err()`: the source's failure, and any error from closing it, reach the returned iterator through `Close`, so returning them from `next` as well would report the same failure twice. `next` may call `src.Next` any number of times per item, so it can drop items or buffer several.

`done`, if non-nil, is called exactly once when the returned iterator closes, with the error `src` ended with, or `nil` if it ended cleanly. It runs after `src` has been closed, so work ended there — a span, a metric, a leak check — covers the whole iteration.

```go
// Keep only events from one stream.
return eventsourcing.Wrap(src, func(_ context.Context, src *eventsourcing.Iterator[*Envelope]) (*Envelope, error) {
    for src.Next() {
        if src.Value().StreamID == want {
            return src.Value(), nil
        }
    }
    return nil, io.EOF
}, nil)
```

---

## IterFunc

```go
type IterFunc[T any] func(ctx context.Context) (T, error)
```

The function that produces the next item of an `Iterator`; see `NewIteratorFunc` for its contract. `ctx` is the context the iterator was created with — a function that blocks must return promptly once it is done.
