# TODO: `event_registry.go`

133 lines. Global name→factory registry for event deserialization.

## Correctness
- [ ] **`typeToNames` is keyed by `fmt.Sprintf("%T", ev)`** (`:112`), so it carries the package-name
      collision from `command_bus.go.md`: two event types in differently-pathed packages sharing a
      package name collapse onto one entry, and `EventNamesFor` returns the wrong type's aliases.
- [ ] **The pointer/value key asymmetry starts here.** `RegisterEventByType`'s factory always returns a
      pointer, so entries are always keyed `"*pkg.T"`. `EventGroupProcessor.StreamFilter` looks them
      up with whatever `OnEvent`'s type parameter produced — see the three failing tests in
      `event_handler.go.md`. Normalising the key (strip the pointer, or key on `reflect.Type`) fixes
      the lookup from this side.

## API
- [ ] **Six package-level mutable `var`s hold the registry and its API.** `RegisterEventByType`,
      `RegisterEventByName`, `NewEventByName` and `EventNamesFor` are declared as **variables**
      holding functions, so any caller can reassign them and change the behaviour process-wide.
      Presumably done for test seams — but the tests reach past them anyway and reset `registry`
      directly (`event_handler_test.go:304-307`). Make them ordinary funcs and, if a seam is needed,
      introduce a `Registry` type with methods plus a package-level default.
- [ ] **Global registry makes tests order-dependent.** Every test that registers has to save and
      restore `registry`/`typeToNames` by hand under `registryMu`. A `Registry` value passed to the
      stores that need it removes the ritual and the cross-test coupling. Guide:
      [global state](https://google.github.io/styleguide/go/best-practices#global-state).
- [ ] **Registration panics in five places** (`:91`, `:95`, `:101`, `:106`, plus the duplicate check).
      Startup-time programmer error is the defensible case for panic, but `registerEventNameDefault`
      panics with bare strings (`"cannot register nil factory"`) rather than errors wrapping a
      sentinel, so nothing can be matched with `errors.Is`. Note `ErrEventNotRegistered` exists and is
      used for the read path — the write path should be equally structured.

## Style
- [ ] **`panic(fmt.Sprintf(...))` should be `panic(fmt.Errorf(...))`** for consistency with
      `command_bus.go:255` and `event_handler.go:116`, which panic with errors.
- [ ] The doc comments live on the `var` declarations inside a single `var (...)` block, which reads
      oddly on pkg.go.dev — the functions render as variables, not as the API they are.

## Good
- **`RegisterEvent[T, PT eventPtr[T]]` is genuinely clever and correct**: it recovers `T` from the
  pointer type parameter so it can mint `new(T)` per decode, instead of closing over the single
  instance the caller passed. The doc says exactly why ("concurrent or repeated decodes never alias
  the same value") — this is the kind of comment that earns its place.
- `RegisterEventByName` existing alongside `RegisterEventByType` is the right escape hatch for renames,
  and `EventNamesFor` returning *all* aliases is what makes subscription filters correct after one.
- Factory results are nil-checked at both registration and construction time.
