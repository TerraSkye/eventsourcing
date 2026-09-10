# TODO: `revision.go`

33 lines. `StreamState` and its four implementations.

## API
- [ ] **`ToRawInt64` leaks a store-specific encoding into the public interface.** `-1` for `Any` and
      `-2` for `StreamExists` are KurrentDB's wire markers; every other store has to translate them
      back with a type switch anyway (see `eventstore/memory/eventstore.go:198`,
      `eventstore/postgres/eventstore.go:204`). Since implementations type-switch regardless, the
      method earns little and invites the bug below.
- [ ] **`Revision uint64` → `ToRawInt64() int64` silently truncates above `MaxInt64`.** Unreachable
      in practice, but the conversion is unchecked.

## Correctness
- [ ] **A nil `StreamState` is representable and panics on use.** `StreamState` is an interface, so
      `nil` satisfies every parameter typed with it. `postgres`'s `default:` branch calls
      `version.ToRawInt64()` and dereferences nil. See `command_handler.go.md` (`WithStreamState(nil)`)
      for the entry point. `[verified]`

## Good
- Four small value types implementing one interface is the right shape; `Any`/`NoStream`/
  `StreamExists` as empty structs cost nothing.
- Doc comments state precisely what each expectation means.
