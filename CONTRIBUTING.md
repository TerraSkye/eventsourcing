# Contributing

Issues and pull requests are welcome. This page says how the repository
works so a change lands with as little back and forth as possible.

## Before proposing a feature

Read [What the library does not do](docs/explanation/non-goals.md) first.
It lists the features left out on purpose, why, and where the door is left
open. Asking for one of them is still fine: open an issue with the use case
and say what the workaround on that page does not cover.

## Layout

The root module `github.com/terraskye/eventsourcing` holds the core and the
`memory` store, and depends only on `uuid` and `backoff`. The drivers and
telemetry are nested Go modules so their dependencies stay out of the root:
`sqlite/`, `postgres/`, `kurrentdb/` and `telemetry/`, plus the two example
applications under `examples/`. The `go.work` file at the root points them
at the local checkout, so a change to the root is visible in the nested
modules without a replace directive.

## Building and testing

The workspace needs the Go version in `go.work`; the root module alone
builds on the version in its `go.mod`.

```bash
# unit tests, all modules (what the Unit Tests workflow runs)
go test -race ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/... ./examples/todolist/... ./examples/dashboard/...

# formatting and vet, including the files that build only under the integration and bench tags
gofmt -l $(git ls-files '*.go')
go vet ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/... ./examples/todolist/...
go vet -tags=integration,bench ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/... ./examples/todolist/...

# root module alone, on its minimum Go version
GOWORK=off go test -race ./...
```

The integration tests sit behind the `integration` build tag. The Postgres
and KurrentDB tests start their database with testcontainers, so they need
a running Docker daemon; the SQLite and memory tests need nothing beyond Go.

```bash
go test -race -tags=integration -timeout 20m ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/...
```

CI runs the unit tests, `gofmt`, `go vet` and the integration tests on every
pull request, and builds the docs with `mkdocs build --strict` when a pull
request touches `docs/` or `mkdocs.yml`. Run the same commands locally
before pushing. Benchmarks are behind the `bench` tag; see
[BENCHMARKS.md](BENCHMARKS.md).

### Adding a store

A new backend goes in its own module, like the existing ones. Run it
against `eventsourcingtest.AcceptanceTest` and
`eventsourcingtest.GlobalReaderAcceptanceTest`, and against
`projectiontest` and `automationtest` if it provides a projection or
automation store. A store that passes them works with projections and
automations unchanged.

## Issues and pull requests

One concern per issue and per pull request. A pull request that fixes a
bug, renames an option and reformats a file is three pull requests. File an
issue first, even for a small change, so the discussion of what to do is
separate from the review of how it was done.

Base every pull request on `master`, never on another pull request's
branch. Pull requests are squashed on merge, so the history of a branch
does not need to be rewritten; push fixup commits as the review goes and
they are folded into one commit at the end. The commit that lands is the
pull request's title and description, so keep those in the commit style
below.

A pull request should contain:

- The change and its tests. A test goes in the `_test.go` file named after
  the source file it covers: a test of `foo.go` belongs in `foo_test.go`.
  Do not add a separate file for a new test.
- A `CHANGELOG.md` entry under `## [Unreleased]` for anything a user can
  see, in the existing style: a bullet starting with the package or type
  it concerns in backticks, saying what changed and what to do about it.
  An API change goes under `### Breaking`, with the old and new names side
  by side; otherwise use `### Added`, `### Changed` or `### Fixed`.
- Updated documentation when the API changed: the doc comments, the pages
  under `docs/` and the readme where they mention it.

## Code style

Code is formatted with `gofmt` and passes `go vet`. Doc comments follow
[go.dev/doc/comment](https://go.dev/doc/comment): a comment on an exported
name starts with that name, says what the thing does and what the caller
must uphold, and is a complete sentence. The doc comments are the primary
reference for the library, so they carry the contracts, not just a
restatement of the signature.

## Commit messages

The subject is one imperative sentence without a prefix, under about 70
characters: `Retry KurrentDB appends that reach a follower`, not
`fix: retry` or `Retrying appends`. The body, wrapped at 72 columns, says
why the change is made and what a reader needs to know that the diff does
not show. The last line names the issue it closes:

```
Pause automations through the store and publish their status

Store.SetPaused pauses an automation for every instance: each stops
claiming at its next poll, lets running items finish and gives its
segments back. The SQL stores keep the pause in automation_controls.

Fixes #123
```

## Releases

Releases are cut by the maintainer. The nested modules are tagged
alongside the root (`sqlite/v0.5.0` with `v0.5.0`) and require the
matching root version, so a change that touches the root and a driver
ships in one release.

## Security

Do not open a public issue for a vulnerability; see [SECURITY.md](SECURITY.md).
