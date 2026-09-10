# Style & correctness TODOs

One file per source file, mirroring the repo layout — `.todos/otel/otel.go.md` covers `otel/otel.go`.
35 files, ~6,800 lines of non-test Go.

Reviewed against [Go Style Best Practices](https://google.github.io/styleguide/go/best-practices.html)
and [Go Style Decisions](https://google.github.io/styleguide/go/decisions). The `otel/` files are
additionally reviewed against the OpenTelemetry semantic conventions.

Headings used throughout:

- **Correctness** — wrong behaviour, data loss, panic, or race.
- **API** — public surface that will be painful to change later.
- **Style** — style guide deviations; safe to batch.
- **Good** — worth preserving. Don't "fix" these.

Items marked `[verified]` were reproduced by running the code.

---

## Start here

Five failing tests on `master`, all genuine defects, all with diagnoses already written:

| Test | File |
|---|---|
| `TestNewCommandHandler_PinnedRevisionSkipsStateFolding` | [`command_handler.go.md`](command_handler.go.md) |
| `TestNewCommandHandler_AutoConvergeDefaultDoesNotPinEmptyStreamRevision` | [`command_handler.go.md`](command_handler.go.md) |
| `TestEventGroupProcessor_StreamFilter_ValueHandlerMissesPointerRegisteredAliases` | [`event_handler.go.md`](event_handler.go.md) |
| `TestStreamFilter_PointerHandlerOfValueReceiverEventPanics` | [`event_handler.go.md`](event_handler.go.md) |
| `TestEventGroupProcessor_StreamFilter_HandlerWithoutEventInstanceIsDropped` | [`event_handler.go.md`](event_handler.go.md) |

Then, in rough order of blast radius:

1. **Silent data loss on read** — `eventstore/kurrentdb` reports every mid-stream failure as a clean
   `io.EOF`, so aggregates rebuild from truncated history and report success.
2. **`Stop()` doesn't wait for handlers** — `command_bus.go`; a command can execute after shutdown
   returns and the DB pool is closed.
3. **Unbounded metric cardinality** — `otel/event_store.go` puts `stream.id` on metrics.
4. **Memory store never persists `GlobalVersion`** — global subscriptions always restart from zero.
5. **File store silently overwrites events** when `Version` is unset (its own TODO).

---

## Index

**Core** ·
[command.go](command.go.md) ·
[command_bus.go](command_bus.go.md) ·
[command_handler.go](command_handler.go.md) ·
[context.go](context.go.md) ·
[errors.go](errors.go.md) ·
[event.go](event.go.md) ·
[event_bus.go](event_bus.go.md) ·
[event_handler.go](event_handler.go.md) ·
[event_registry.go](event_registry.go.md) ·
[eventstore.go](eventstore.go.md) ·
[iter.go](iter.go.md) ·
[middleware.go](middleware.go.md) ·
[query_bus.go](query_bus.go.md) ·
[query_gateway.go](query_gateway.go.md) ·
[query_handler.go](query_handler.go.md) ·
[revision.go](revision.go.md) ·
[version.go](version.go.md)

**eventstore/** ·
[memory](eventstore/memory/eventstore.go.md) ·
[file](eventstore/file/filestorage.go.md) ·
[postgres](eventstore/postgres/eventstore.go.md) ·
[kurrentdb](eventstore/kurrentdb/eventstore.go.md)

**eventbus/** ·
[memory](eventbus/memory/eventbus.go.md) ·
[file](eventbus/file/eventbus.go.md) ·
[postgres](eventbus/postgres/eventbus.go.md) ·
[kurrentdb](eventbus/kurrentdb/eventbus.go.md)

**logging/** ·
[command_handler.go](logging/command_handler.go.md) ·
[event_handler.go](logging/event_handler.go.md) ·
[query_handler.go](logging/query_handler.go.md)

**otel/** ·
[otel.go](otel/otel.go.md) ·
[config.go](otel/config.go.md) ·
[command_handler.go](otel/command_handler.go.md) ·
[query_handler.go](otel/query_handler.go.md) ·
[event_handler.go](otel/event_handler.go.md) ·
[event_bus.go](otel/event_bus.go.md) ·
[event_store.go](otel/event_store.go.md)

---

## Cross-cutting themes

These recur across many files. Fixing them at the source closes a lot of individual items at once.

### 1. `fmt.Sprintf("%T", x)` as a type key
Used in `command_bus.go`, `event_handler.go`, `event_registry.go`, `otel/command_handler.go`,
`otel/query_handler.go`. It **is** reflection (`fmt` calls `reflect.TypeOf` for `%T`) — the slowest,
lossiest form:

```
BenchmarkSprintfTypeKey-16      76.79 ns/op    24 B/op    1 allocs/op
BenchmarkReflectTypeKey-16      12.38 ns/op     0 B/op    0 allocs/op
BenchmarkSelfDeclaredName-16     9.25 ns/op     0 B/op    0 allocs/op
```

It yields `"<nil>"` for an interface type parameter, and it prints the *short* package name, so
`a/foo.Command` and `b/foo.Command` collide. `query_bus.go` already fixed its half (commit 12b5592).
Full discussion and decision in [`command_bus.go.md`](command_bus.go.md); the structural fix is
`Command.CommandName()`, mirroring `Event.EventType()`.

### 2. The exclusive `LoadStreamFrom` convention is undocumented
Every store implements `LoadStreamFrom(id, Revision(N))` as `Version > N`, and
`NewCommandHandler`'s retry depends on it. `eventstore.go`'s doc says "starting at version", which
reads inclusive. One sentence in the interface contract — see [`eventstore.go.md`](eventstore.go.md).

### 3. `Iterator` has no `Close`
Costs a leaked pgx connection per abandoned iterator in `eventstore/postgres`.
[`iter.go.md`](iter.go.md), [`eventstore.go.md`](eventstore.go.md),
[`eventstore/postgres`](eventstore/postgres/eventstore.go.md).

### 4. `SubscriberOption func(cfg any)`
Every implementation's options panic on a type mismatch, and nothing catches it at compile time.
Six panic sites across the four buses. [`event_bus.go.md`](event_bus.go.md) —
`otel/config.go`'s `Option` interface is the pattern to copy.

### 5. Sentinel errors documented but not used
`errors.go` says `ErrDuplicateHandler` is returned by "[EventBus.Subscribe] implementations". None of
the four buses wrap it, and each uses a different message for the same condition
(`"handler with name %q already registered"` / `"subscriber %q already exists"` /
`"bus is closed"` / `"eventbus is closed"`). [`errors.go.md`](errors.go.md).

### 6. Malformed error strings
`ErrDuplicateHandler` has a trailing space; `"business rule violation :%s"` has the space on the
wrong side of the colon *and* is double-prefixed by `command_handler.go`;
`ErrHandlerNotRegistered` dangles ("...for type"); `command_bus.go` wraps the not-registered error
twice. All `[verified]`. [`errors.go.md`](errors.go.md).

### 7. `StreamRevisionConflictError.Error()` panics on nil revisions
Confirmed in `eventstore/kurrentdb`'s own TODO. Anything logging such an error crashes, and
`command_handler.go` silently reports `NextExpectedVersion: 0`. [`errors.go.md`](errors.go.md).

### 8. Errors discarded on paths where they mean data loss
`otel/otel.go` drops all 13 instrument-construction errors; both postgres files silently swallow a
metadata unmarshal failure; `eventstore/file` skips unreadable and undecodable event files with
`continue`; `eventbus/file` drops a failed rename.

### 9. Duplicated implementations that have drifted
Every `otel` wrapper exists twice with divergent span names, SpanKinds and metrics; `scanEnvelope` is
byte-identical across `eventstore/postgres` and `eventbus/postgres`; the three `LoadStream*` iterator
wrappers in `otel/event_store.go`; the two command/query telemetry pairs.

### 10. Structured-log key conventions
Three schemes across one `logging` package: `aggregateID`, `aggregateId`, `stream-id`.
[`logging/event_handler.go.md`](logging/event_handler.go.md).

---

## What is consistently good

Worth stating, because the review format above only lists problems:

- **The doc comments are the strongest part of this codebase.** `command.go`'s intent-vs-implementation
  table, `queryKey`'s explanation of the `(*T)(nil)` trick, `command_bus.go`'s `stopCh`/`enqueueWG`/
  `drainCh` interlock, `query_gateway.go`'s buffered-channel reasoning, `eventstore/file`'s rollback
  rationale, and `eventbus/postgres`'s `newLockPool` all explain *why*, and each one would stop a
  future reader from "simplifying" something load-bearing.
- **The TODOs are honest and specific.** The kurrentdb files in particular state symptom, mechanism
  and user-visible consequence, and mark what was confirmed. Most items in those two files were
  already diagnosed by whoever wrote them.
- **The test suite is ahead of the implementation.** Five failing tests exist specifically to pin down
  bugs, with `.bug/` write-ups referenced. That is an unusually healthy state.
- **Concurrency designs are sound where it counts** — per-aggregate sharding, advisory locks, MVCC
  snapshot filtering, `context.WithoutCancel` for in-flight handlers, buffered response channels.
