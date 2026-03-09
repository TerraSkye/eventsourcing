# Iterator

```go
type Iterator[T any] struct { ... }
```

A generic, lazy iterator over items of type `T`. Used by `EventStore` to stream events without loading them all into memory.

## Methods

### Next

```go
func (it *Iterator[T]) Next(ctx context.Context) bool
```

Advances to the next item. Returns `true` if a value is available, `false` at end-of-stream or on error. Respects context cancellation.

### Value

```go
func (it *Iterator[T]) Value() T
```

Returns the current item. Only valid after `Next` returns `true`.

### Err

```go
func (it *Iterator[T]) Err() error
```

Returns the error encountered during iteration, or `nil` if iteration completed normally. Always check after the loop.

### All

```go
func (it *Iterator[T]) All(ctx context.Context) ([]T, error)
```

Consumes the iterator and returns all items as a slice. Convenience method for small datasets.

---

## Usage pattern

```go
iter, err := store.LoadStream(ctx, streamID)
if err != nil {
    return err
}

for iter.Next(ctx) {
    envelope := iter.Value()
    state = evolve(state, envelope)
}
if err := iter.Err(); err != nil {
    return err
}
```

---

## Constructors

### NewIteratorFunc

```go
func NewIteratorFunc[T any](nextFunc func(ctx context.Context) (T, error)) *Iterator[T]
```

Creates an `Iterator[T]` from a function. The function must return `(T, nil)` for each item, and `(zero, io.EOF)` to signal end of stream.

```go
i := 0
iter := eventsourcing.NewIteratorFunc(func(ctx context.Context) (int, error) {
    if i >= len(items) {
        return 0, io.EOF
    }
    v := items[i]
    i++
    return v, nil
})
```

### NewSliceIterator

```go
func NewSliceIterator[T any](slice []T) *Iterator[T]
```

Creates an `Iterator[T]` from an existing slice. Useful for testing and in-memory implementations.

```go
iter := eventsourcing.NewSliceIterator(envelopes)
```
