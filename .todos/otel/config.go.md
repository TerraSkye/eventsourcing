# TODO: `otel/config.go`

72 lines. The `Option` type and the four span-customisation options.

## API
- [ ] **Missing `WithMeterProvider` / `WithTracerProvider`.** This file is the natural home for them,
      and their absence is what pins the package to the global providers. See `otel/otel.go.md` —
      this is the single change that makes the package testable against a local `SpanRecorder`.
- [ ] **`WithAttributes` overwrites rather than appends** (`:62`): `o.Attributes = attrs`. Two calls,
      or a call combined with a preset, silently discard the first. Every other option in the repo
      that takes a list appends (`WithMetadataExtractor` in `command_handler.go`). Use
      `append(o.Attributes, attrs...)`.
- [ ] **`WithOperationGetter` and `WithAttributeGetter` replace rather than compose**, so only the last
      one registered runs. Either document that explicitly or chain them.
- [ ] **`config` fields are exported on an unexported struct** (`Operation`, `GetOperation`,
      `Attributes`, `GetAttributes`). Harmless, but inconsistent with `handlerOptions` in the root
      package, which does the same — worth being deliberate rather than accidental.

## Good
- **This is the cleanest API design in the repo.** `Option` as an interface with an unexported `apply`
  method keeps the option set closed to outside implementations — the correct functional-options
  pattern, and better than the bare `func(*T)` used elsewhere in the codebase.
- `optionFunc` adapter is the standard idiom, correctly applied.
- Every field documents its nil/empty behaviour ("If it returns an empty string, the existing
  operation name is used instead"), which is exactly what the caller needs to know.
- This is the pattern `eventsourcing.SubscriberOption func(cfg any)` should adopt — see
  `event_bus.go.md`.
