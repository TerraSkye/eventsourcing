# TODO: `iter.go`

113 lines. The generic pull `Iterator[T]` used by every `EventStore` Load method.

## Correctness
- [x] **`err == io.EOF` should be `errors.Is(err, io.EOF)`** (`iter.go:52`). An `IterFunc` that wraps
      its sentinel — `fmt.Errorf("read page: %w", io.EOF)`, which is the natural thing to write for a
      paginating source — is classified as a **failure** instead of a clean end of iteration. The doc
      on `IterFunc` says "returning [io.EOF] once iteration is complete", which invites the wrap.
      Fixed in `iter.go` (now `iter.go:59`) and at `otel/event_store.go:167`. **Still open** at
      `otel/event_store.go:279` — tracked in [`otel/event_store.go.md`](otel/event_store.go.md).
- [x] **`it.err = nil` in the EOF branch is a redundant assignment** (`iter.go:54`) — `it.err` is
      guaranteed nil there by the guard at `:44`. Harmless, but it reads as if it might not be.
      Removed; the EOF branch now only sets `done`.

## API
- [ ] **No `Close`.** A consumer that stops early — which `command_handler.go:181` does on
      `iter.Err()` — has no way to release the underlying resource, and a postgres-backed iterator
      holds a cursor. Either add `Close() error` or state on `EventStore` that iterators must be
      drained. Tracked from the other side in `eventstore.go.md`.
- [x] **Not safe for concurrent use, and doesn't say so.** `current`, `err` and `done` are unsynchronised,
      and `NewSliceIterator`'s closure captures a plain `index`. One line of doc prevents a real bug.
      Stated on the `Iterator` type doc — covers the source-side state (`NewSliceIterator`'s `index`)
      as well, since `Next` is the only caller of the `IterFunc`.
- [x] **`All` gives no way to distinguish "consumed nothing" from "already exhausted".** Calling it on
      a partly-consumed iterator silently returns only the remainder. Worth a sentence.
      Documented on `All`: it resumes from the current position, returns nil on an exhausted
      iterator, and still returns the items collected before a failure.

## Good
- The three-way `IterFunc` contract — `(T, nil)` / `io.EOF` / other error — is documented right on the
  field where an implementer will read it, with all three cases enumerated.
- The `Next`/`Value`/`Err` triple is the `bufio.Scanner` shape, so it needs no explanation to a Go
  reader, and `Err`'s doc correctly explains how to tell exhaustion from failure.
- Zeroing `current` before returning false stops a stale value being read after the iteration ends.
