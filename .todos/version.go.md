# TODO: `version.go`

62 lines. Reports this module's version to the `otel` subpackage.

## Done — all three items fixed

- [x] **Version is now derived, not hand-maintained.** `InstrumentationVersion` reads the module's
      version from the build information the Go toolchain embeds in the consuming binary, so it can
      no longer drift from the tag. Verified both paths:
      - consumer binary → the real version from `BuildInfo.Deps`
      - this repository's own tests → `"(devel)"` from `BuildInfo.Main`
      - missing build info, or a module recorded with no version → `"unknown"`
      Replace directives are followed to the module that actually supplies the code.
- [x] **Parenthesized single-constant block removed.**
- [x] **Doc comment moved onto the identifier**, and expanded to state the two non-release values a
      caller can observe.

Covered by `version_test.go`: table tests over synthetic `debug.BuildInfo` and `debug.Module` values
for the main-module, dependency, absent, no-version, replaced and chained-replacement cases.

## Follow-up: `const` became `var`

A runtime-computed value cannot be a `const`, so `InstrumentationVersion` is now a `var`. Two
consequences:

- **Source-breaking for any caller using it in a constant expression.** No such use exists in this
  repo, and a version string is an unlikely constant, but it is technically a breaking change — worth
  landing with the next minor version rather than a patch.
- **It is now writable by callers.** Low stakes for a version string, and arguably useful as an
  override hook, but it is the same exported-mutable-`var` pattern flagged in
  [`otel/otel.go.md`](otel/otel.go.md).

If either matters, the alternative is `func Version() string`, which is what the OpenTelemetry
instrumentation libraries themselves use — though they pair it with hand-maintained constants and
release tooling that bumps them, which is the chore this change removes. Changing to a function form
would need the two call sites in `otel/otel.go` updated.
