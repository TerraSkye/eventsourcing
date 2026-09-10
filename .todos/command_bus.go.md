# TODO: `command_bus.go`

295 lines. `CommandBus`, `Dispatch`, `Register`, `Stop`, and the shard workers.

`gofmt`/`vet` clean; the 16 tests in `command_bus_test.go` pass under `-race`. Everything below was
reproduced by running the code.

## Correctness — shutdown
- [ ] **`Stop()` returns while a handler is still executing.** `b.wg` counts **`Dispatch` calls**
      (`Add` at `:119`, `Done` at `:122`), not handler executions; the worker goroutines started at
      `:78` have no `WaitGroup` at all. Normally masked because `Dispatch` blocks on `responseCh` —
      but the `ctx.Done()` abandonment path at `:147` breaks that, and the handler then runs
      untracked:
      ```
      Dispatch returned early with: ... context canceled
      Stop() RETURNED while handler is still running (finished=false)
      handler finished = true          <- afterwards
      ```
      `[verified]`
- [ ] **A queued command executes *after* `Stop()` has returned.** Same cause. A command sitting in a
      shard buffer whose caller walked away is drained and run after `Stop` returns:
      ```
      Stop() returned; queued command executed = false
      AFTER Stop returned, queued command executed = true
      ```
      So a process can `Stop()` the bus, close its database pool, and then have a handler write
      through it. `[verified]`
- [ ] **Fix for both:** add `workerWG sync.WaitGroup`; `Add(1)` before each `go bus.worker(...)`,
      `defer workerWG.Done()` in `worker`, and have `Stop` wait on it after closing `drainCh`. That is
      the one missing link in an interlock that is otherwise carefully built.

## Correctness — routing and registration
- [ ] **`selectShard` returns a negative index on 32-bit platforms** (`:232`).
      `int(hash.Sum32()) % b.shardCount` — where `int` is 32 bits (`386`, `arm`, `wasm`), any hash
      above `MaxInt32` (roughly half of all inputs) converts negative, and Go's `%` keeps the sign:
      ```
      "a"    sum32=3826002220 -> int32=-468965076   % 4 =  0
      "zzz"  sum32=2813343901 -> int32=-1481623395  % 4 = -3   <- index out of range
      ```
      Fix: `int(hash.Sum32() % uint32(b.shardCount))`. `[verified]`
- [ ] **`Register` with an interface type parameter registers under `"<nil>"` and never fires**
      (`:249-250`). `var zero C` for an interface `C` is a nil interface with no dynamic type, so
      `%T` renders `"<nil>"`, while the worker keys on the concrete type name:
      ```
      registry key = "<nil>"
      dispatch err = ... no handler registered for type ; handler called = false
      ```
      **This is the same bug class already fixed for `QueryBus` in commit 12b5592**; `CommandBus` was
      not covered. Note the `(*T)(nil)` trick used there does not transplant, because this key must
      match a name derived from a concrete runtime value. See the decision note below. `[verified]`
- [ ] **`bufferSize` is not validated while `shardCount` is** (`:63` vs `:77`).
      `NewCommandBus(-1, 2)` panics with `makechan: size out of range`. `[verified]`

## Correctness — error handling
- [ ] **Every handler-not-registered error carries its prefix twice.** The worker wraps at `:180-183`
      and `Dispatch` wraps the same error again at `:144`:
      ```
      dispatch command eventsourcing.testEvent for aggregate "a": dispatch command eventsourcing.testEvent for aggregate "a": no handler registered for type
      ```
      The worker already has the type and aggregate ID, so `Dispatch` should pass `result.Err`
      through unwrapped. `[verified]`
- [ ] **`errors.Join` puts a newline inside the error string** (`:200`), breaking single-line log
      ingestion:
      `... panic: boom\nhandler panicked when handling command`.
      `fmt.Errorf("%w: %w", ErrHandlerPanicked, panicErr)` unwraps identically on one line.
      `[verified]`
- [ ] **The duplicate-registration panic message is malformed** (`:255`): no separator between `%s`
      and `%w`, and `ErrDuplicateHandler` itself ends in a space (see `errors.go.md`):
      `handler already registered for command type eventsourcing.testEvent duplicate handler registered `
      `[verified]`
- [ ] **The panic recovery discards the stack trace** (`:189-211`). Capture `debug.Stack()` at recover
      time — without it a handler panic reports `panic: boom` with no location. The stale
      `//TODO improve the error` at `:204` is a marker for this.

## API
- [ ] **`Register` panics, and its documented example does not compile.** `:247` shows
      `err := Register(bus, fooHandler)` but `Register` returns nothing. Either return an error and
      update the example, or fix the example and state that it panics on duplicates.

## Style
- [ ] Four identical `fmt.Errorf("dispatch command %T for aggregate %q: %w", ...)` calls (`:116`,
      `:135`, `:138`, `:144`, `:148`); longest line is 168 chars. Extract `dispatchErr(cmd, err)`.
- [ ] `for i := 0; i < shardCount; i++` (`:76`) — module targets Go 1.25, so `for range shardCount`.
- [ ] `make([]CommandHandlerMiddleware, 0)` (`:70`) can be nil.
- [ ] `worker` never checks `cmd.Ctx.Err()` before invoking the handler (`:213`), so a backlog of
      timed-out commands still executes in full.
- [ ] `fmt.Sprintf("%T", cmd.Command)` (`:173`) allocates per command on the hot path.
- [ ] `NewCommandBus(bufferSize, shardCount int)` — the doc sentence introduces them in the reverse
      order of the signature. Two adjacent `int`s that are easy to swap.

## Decision needed: how to key the registry
Discussed and settled in conversation — recorded here so the fix isn't re-litigated:

`fmt.Sprintf("%T", x)` **is** reflection (`fmt` calls `reflect.TypeOf` for `%T`), so the current code
does not avoid reflect — it uses the slowest and lossiest form of it:

```
BenchmarkSprintfTypeKey-16      76.79 ns/op    24 B/op    1 allocs/op   <- today
BenchmarkReflectTypeKey-16      12.38 ns/op     0 B/op    0 allocs/op
BenchmarkSelfDeclaredName-16     9.25 ns/op     0 B/op    0 allocs/op
```

`%T` also prints the *short* package name, so two command types in differently-pathed packages that
share a package name produce the **same key** (`a/foo.Command` and `b/foo.Command` both render
`"foo.Command"`). `[verified]`

**Preferred fix:** give `Command` a `CommandName() string`, mirroring `Event.EventType()` which this
codebase already has. Fastest of the three, makes the `<nil>` key unrepresentable, removes the
package-name collision, and gives a stable wire name if commands ever arrive from a queue. Breaking
change; and because `var zero C` is unsafe for pointer types, registration should take the name
explicitly (`RegisterCommand(bus, "ReserveSeat", handler)`), mirroring `RegisterEventByName`.

**Interim if the break isn't acceptable:** key on `reflect.Type`. Note this is *less* reflection than
today, not more — it drops the `fmt` formatting layer.

## Good
- The comments at `:96-111` and `:279-292` explaining the `stopCh`/`enqueueWG`/`drainCh` interlock are
  excellent. That protocol is subtle and the reasoning is precise and correct.
- Sharding by aggregate ID with one worker per shard is the right concurrency model — per-aggregate
  serialization without a global lock.
- `responseCh` is buffered with capacity 1, so a worker never blocks writing to a caller that has
  walked away. Load-bearing and easy to get wrong.
- `handlerFor` holds the lock only across the map read, never across the handler call.
