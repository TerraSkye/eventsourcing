# TODO: `eventstore.go`

57 lines. The `EventStore` interface and `AppendResult`.

## Correctness
- [ ] **`LoadStreamFrom`'s doc does not say whether `version` is inclusive or exclusive.** It reads
      "returns an iterator over id's stream **starting at** version", which reads inclusive. Every
      implementation in this repo is **exclusive** (returns events with `Version > N`), and
      `NewCommandHandler`'s retry loop silently depends on that: it folds incrementally into state it
      does not reset between attempts. A third-party `EventStore` that implemented the doc as written
      would double-apply the boundary event into every aggregate on every retry.
      **This is the highest-value one-line doc fix in the repo.** State the convention here, in the
      interface contract, where implementers will read it. `[verified]`

## API
- [ ] **`AppendResult.Successful` duplicates `err != nil`.** Two sources of truth for one fact, and
      the codebase already disagrees with itself about which to set (`command_handler.go` returns
      `Successful: false` alongside errors, `Successful: true` with no save at all for the zero-event
      case). Either drop the field or document precisely when it may differ from the error.
- [ ] **`Iterator` has no `Close`.** A consumer that abandons an iterator mid-stream (e.g. on
      `iter.Err()`) leaves the backing cursor open — real for the postgres store. Either add `Close`
      to `Iterator` or state on this interface that iterators must be drained.

## Good
- The ordering guarantee ("oldest first — from every Load* method") is stated once, on the interface,
  rather than repeated per method.
- `LoadFromAll` explicitly documents that cross-stream ordering is implementation-specific instead of
  over-promising. That is the right call and rare.
- `Close` documents the idempotency expectation.
