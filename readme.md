# eventsourcing

Event sourcing and CQRS for Go, built around a functional `decide`/`evolve` core, typed buses, a projection runner, and pluggable stores for SQLite, Postgres and KurrentDB.

[![Go Reference](https://pkg.go.dev/badge/github.com/terraskye/eventsourcing.svg)](https://pkg.go.dev/github.com/terraskye/eventsourcing)
[![Unit Tests](https://github.com/TerraSkye/eventsourcing/actions/workflows/unit-tests.yml/badge.svg)](https://github.com/TerraSkye/eventsourcing/actions/workflows/unit-tests.yml)
[![Integration Tests](https://github.com/TerraSkye/eventsourcing/actions/workflows/integration-tests.yml/badge.svg)](https://github.com/TerraSkye/eventsourcing/actions/workflows/integration-tests.yml)
[![License: MPL-2.0](https://img.shields.io/badge/license-MPL--2.0-blue.svg)](LICENSE)

**[Documentation](https://terraskye.github.io/eventsourcing/)** · [Tutorials](https://terraskye.github.io/eventsourcing/tutorials/) · [Example app](examples/todolist) · [Changelog](CHANGELOG.md)

---

## Why this library

- **Aggregates are two pure functions.** `evolve(state, event) → state` rebuilds state from history; `decide(state, command) → []event` enforces the rules. No base struct to embed, no reflection, trivial to unit test.
- **Generics, not `interface{}`.** Command handlers, query handlers and event handlers are typed end to end. A handler for `CreateTask` can't be called with anything else.
- **Optimistic concurrency built in.** Every save states an expectation (`Any`, `NoStream`, `StreamExists`, `Revision(n)`); conflicts come back as a typed error, and handlers can retry with backoff.
- **Projections that catch up, checkpoint and rebuild.** A `projection.Runner` reads the global log in order, keeps a checkpoint per read model, rebuilds on a version bump, and can be segmented across instances when one can't keep up.
- **Stores you can grow into.** Start with `memory` in tests and `sqlite` on your laptop; move to `postgres` or `kurrentdb` when several processes share the log. Every store passes the same acceptance suite.
- **Batteries included, dependencies isolated.** OpenTelemetry traces and metrics, `slog` middleware and a test suite for stores ship in the repo. Drivers live in separate Go modules, so the root module pulls in only `uuid` and `backoff`.

## Install

```bash
go get github.com/terraskye/eventsourcing
```

The root module needs Go 1.24+. Backends and telemetry are separate modules, fetched only when you use them:

```bash
go get github.com/terraskye/eventsourcing/sqlite
go get github.com/terraskye/eventsourcing/postgres
go get github.com/terraskye/eventsourcing/kurrentdb
go get github.com/terraskye/eventsourcing/telemetry
```

## Quick start

A task that can be created once. Events and commands are plain structs; the handler is assembled from `evolve` and `decide`.

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
)

// --- Event: something that happened. Pointer receivers, registered once. ---

type TaskCreated struct {
	TaskID uuid.UUID `json:"task_id"`
	Title  string    `json:"title"`
}

func (e *TaskCreated) AggregateID() string { return e.TaskID.String() }
func (e *TaskCreated) EventType() string   { return "TaskCreated" }

func init() { eventsourcing.RegisterEvent(&TaskCreated{}) }

// --- Command: something someone wants to happen. ---

type CreateTask struct {
	TaskID uuid.UUID
	Title  string
}

func (c CreateTask) AggregateID() string { return c.TaskID.String() }
func (c CreateTask) CommandType() string { return "CreateTask" }

// --- Aggregate: state folded from events, rules checked against it. ---

type taskState struct{ Exists bool }

func evolve(s taskState, env *eventsourcing.Envelope) taskState {
	switch env.Event.(type) {
	case *TaskCreated:
		s.Exists = true
	}
	return s
}

func decide(s taskState, cmd CreateTask) ([]eventsourcing.Event, error) {
	if s.Exists {
		return nil, errors.New("task already exists")
	}
	if cmd.Title == "" {
		return nil, errors.New("title is required")
	}
	return []eventsourcing.Event{&TaskCreated{TaskID: cmd.TaskID, Title: cmd.Title}}, nil
}

func main() {
	ctx := context.Background()

	store := memory.NewEventStore()
	defer store.Close()

	bus := eventsourcing.NewCommandBus(20, 1)
	defer bus.Stop()

	handler := eventsourcing.NewCommandHandler(
		store,
		func() taskState { return taskState{} },
		evolve,
		decide,
	)
	eventsourcing.Register(bus, handler)

	id := uuid.New()
	res, err := bus.Dispatch(ctx, CreateTask{TaskID: id, Title: "Write a README"})
	fmt.Println(res.Successful, err) // true <nil>

	_, err = bus.Dispatch(ctx, CreateTask{TaskID: id, Title: "Again"})
	var violation *eventsourcing.BusinessRuleViolationError
	fmt.Println(errors.As(err, &violation)) // true
}
```

On each dispatch the handler loads the stream, folds it through `evolve`, calls `decide`, wraps the returned events in envelopes (UUID, version, timestamp, metadata) and saves them with the revision it loaded. Handler options let you pin the expected revision, retry on conflict, stamp metadata from the context, or change how streams are named:

```go
eventsourcing.NewCommandHandler(store, initial, evolve, decide,
	eventsourcing.WithStreamState(eventsourcing.NoStream{}),       // must not exist yet
	eventsourcing.WithRetryStrategy(func() backoff.BackOff { // retry save conflicts
		return backoff.WithMaxRetries(&backoff.ZeroBackOff{}, 3)
	}),
	eventsourcing.WithMetadataExtractor(func(ctx context.Context) map[string]any {
		return map[string]any{"user_id": userFromContext(ctx)}
	}),
	eventsourcing.WithStreamNamer(func(ctx context.Context, cmd eventsourcing.Command) string {
		return tenantFromContext(ctx) + "-" + cmd.AggregateID()
	}),
)
```

### Read models

A projection is a `Definition` — a name, a handler, and the event types it cares about — run by a `Runner` against any store that implements `GlobalReader` (all of them do).

```go
import "github.com/terraskye/eventsourcing/projection"

titles := map[string]string{}

group := eventsourcing.NewEventGroupProcessor(
	eventsourcing.OnEvent(func(ctx context.Context, e *TaskCreated) error {
		titles[e.TaskID.String()] = e.Title
		return nil
	}),
)

runner := projection.NewRunner(projection.Definition{
	Name:       "task-titles",
	Handler:    group,
	EventTypes: group.StreamFilter(),
}, store, memory.NewProjectionStore())

go runner.Run(ctx)

// After a dispatch, wait until the read model reflects it:
res, _ := bus.Dispatch(ctx, CreateTask{TaskID: uuid.New(), Title: "Ship it"})
_ = runner.WaitUntil(ctx, res.GlobalPosition)
```

The runner polls the log in batches, commits the checkpoint with each batch (inside the same transaction as your writes on SQLite and Postgres), and rebuilds the read model when you bump `Definition.Version`. Set `Segments` to spread one projection across several instances. See [Part 4 of the tutorial](https://terraskye.github.io/eventsourcing/tutorials/04-projections/) and [Rebuild without downtime](https://terraskye.github.io/eventsourcing/how-to/rebuild-without-downtime/).

### Queries

The query side mirrors the command side with a typed `QueryBus` and `QueryGateway[Q, R]`, which takes middleware with `Use`. See the [reference](https://terraskye.github.io/eventsourcing/reference/interfaces/). Event handlers run under the projection runner above; wrap one in middleware such as `logging.WithEventLogging` before passing it as `Handler`.

## Choosing a store

| Package | Use it for | Event store | Projection store | Notes |
|---|---|:-:|:-:|---|
| `memory` | Unit tests of handlers and aggregates | ✓ | ✓ | In the root module. Not durable. |
| `sqlite` | Local development, tutorials, tests that need real transactions, single-process services | ✓ | ✓ | Pure Go driver (`modernc.org/sqlite`), no C toolchain. One file, or `":memory:"`. WAL mode; replicate with Litestream. |
| `postgres` | Production with several processes sharing the log | ✓ | ✓ | `pgx/v5`. Appends are serialized under an advisory lock so the global log has no gaps; expect lower write throughput on a very busy store. |
| `kurrentdb` | Production on KurrentDB / EventStoreDB | ✓ | – | Uses the official Go client. Projections keep their checkpoints in another package's projection store. |

Every store is run against the shared acceptance suite in [`eventsourcingtest`](eventsourcingtest), which checks ordering, revisions, concurrent saves, paging, filters and iterator cleanup.

**SQLite in three lines:**

```go
db, err := sqlite.Open("events.db") // or ":memory:"
_, err = db.ExecContext(ctx, sqlite.Schema)
store := sqlite.NewEventStore(db)
```

**Postgres:**

```go
pool, err := pgxpool.New(ctx, dsn)
_, err = pool.Exec(ctx, postgres.Schema)
store := postgres.NewEventStore(pool)
```

Projection handlers on SQLite and Postgres write through `sqlite.Tx(ctx)` / `postgres.Tx(ctx)` so the read model and the checkpoint commit together.

## Package map

| Package | What it holds |
|---|---|
| `eventsourcing` (root) | `Command`, `Event`, `Envelope`, `EventStore`, `GlobalReader`, `EventHandler`, command/query buses, `NewCommandHandler`, middleware, event registry, `Iterator`, stream states and typed errors |
| `projection` | `Definition`, `Runner`, `Store`; checkpoints, leases, segments, rebuilds, replay, error policies, dead-lettering |
| `automation` | Background workers that claim items from a todo-list read model and act on them (process managers, scheduled follow-ups) |
| `memory`, `sqlite`, `postgres`, `kurrentdb` | One package per backend, each with `NewEventStore` and, where supported, `NewProjectionStore` |
| `telemetry` | OpenTelemetry spans and metrics for command handlers, query handlers, event handlers, event stores and projections |
| `logging` | `slog` middleware for commands, queries and events |
| `eventsourcingtest`, `projection/projectiontest`, `automation/automationtest` | Acceptance suites and fakes for testing your own implementations |

## Observability

Wrap handlers and stores, or add telemetry as bus middleware:

```go
import "github.com/terraskye/eventsourcing/telemetry"

bus.Use(telemetry.CommandTelemetry())
store := telemetry.WithEventStoreTelemetry(sqlite.NewEventStore(db))
telemetry.ObserveProjection(runner) // lag, position and phase as gauges, dead-lettered events as a counter
```

Spans carry `eventsourcing.command.type`, `eventsourcing.aggregate.id`, `eventsourcing.stream.id`, `eventsourcing.event.type`, `eventsourcing.event.global_position` and friends; metrics cover durations, in-flight counts, failures, concurrency conflicts, and events appended and loaded. Full list in [Add OpenTelemetry](https://terraskye.github.io/eventsourcing/how-to/add-opentelemetry/).

For plain logging, `logging.CommandLogging(logger)`, `logging.QueryLogging(logger)` and `logging.EventLogging(logger)` do the same with `log/slog`.

## Example application

[`examples/todolist`](examples/todolist) is the application built across the tutorials: an HTTP API organised as vertical slices, with a live task-list projection and a background automation that archives completed tasks. It's its own module, so its dependencies stay out of the library's `go.mod`.

```bash
cd examples/todolist
go run .
# then: curl -X POST localhost:9000/api/v1/tasks -H 'Content-Type: application/json' -d '{"title":"Try it"}'
```

## Documentation

The [docs site](https://terraskye.github.io/eventsourcing/) follows the Diátaxis layout:

- **Tutorials** — build the todolist API from the first command to background processing.
- **How-to guides** — concurrency, metadata, multi-tenant stream names, event versioning, middleware, testing, rebuilding projections without downtime, KurrentDB.
- **Explanation** — event sourcing, CQRS, decide/evolve, concurrency, vertical slice architecture.
- **Reference** — every interface, option and backend.

The Go doc comments are the primary reference and spell out the contracts each interface must uphold — including, for `GlobalReader`, why Postgres positions can become visible out of order and how the store prevents it.

## Status and versioning

This is pre-1.0 software under active development by a single maintainer. It is well tested and documented, but:

- A minor release may break the API. Every break is listed under **Breaking** in the [changelog](CHANGELOG.md), with the old and new names side by side.
- Nested modules (`sqlite`, `postgres`, `kurrentdb`, `telemetry`) are tagged alongside the root (`sqlite/v0.2.0` with `v0.2.0`) and require the matching root version. [RELEASING.md](RELEASING.md) describes how.
- Pin a version and read the changelog before upgrading.

## Development

```bash
# unit tests, all modules
go test -race ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/... ./examples/todolist/...

# integration tests (Postgres and KurrentDB via testcontainers; needs Docker)
go test -race -tags=integration -timeout 20m ./... ./postgres/... ./kurrentdb/... ./telemetry/... ./sqlite/...

# benchmarks (Postgres and KurrentDB via testcontainers; see BENCHMARKS.md)
go test -tags=bench -run '^$' -bench . -benchmem -timeout 60m ./memory/ ./sqlite/ ./postgres/ ./kurrentdb/

# root module alone, on its minimum Go version
GOWORK=off go test -race ./...

# docs
pip install -r requirements-docs.txt && mkdocs serve
mkdocs build --strict   # what CI runs on pull requests
```

The repository's `go.work` points the nested modules at the local root; outside the repository they resolve the published version.

Issues and pull requests are welcome. If you're adding a backend, run it against `eventsourcingtest.AcceptanceTest` and `eventsourcingtest.GlobalReaderAcceptanceTest` first; if it passes those, projections and automations work on top of it unchanged.

## License

[Mozilla Public License 2.0](LICENSE)
