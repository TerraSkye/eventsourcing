# Benchmarks

The benchmark suite measures every backend with the same code, so a design
decision can rest on a figure instead of an argument. It was written for
the Postgres append lock, which serializes appends so that the global log
has no gaps: the changelog said "write throughput goes down", and this
suite says by how much. Later changes to the stores (correlation columns,
snapshots, inline projections) can quote before and after numbers from it.

## Running

The benchmarks are behind the `bench` build tag, so `go test ./...` stays
fast. One command runs them on every backend:

```bash
go test -tags=bench -run '^$' -bench . -benchmem -count=6 -timeout 60m \
    ./memory/ ./sqlite/ ./postgres/ ./kurrentdb/ | tee new.txt
go run golang.org/x/perf/cmd/benchstat@latest new.txt
```

`-run '^$'` skips the unit tests of the same packages. Name one package to
measure one backend, and `-bench 'EventStore/Append'` to run one group.
To compare a change, run the same command on both commits and pass both
files to benchstat: `benchstat old.txt new.txt`.

The Postgres and KurrentDB benchmarks start their database in Docker, with
the same testcontainers setup as the integration tests: those packages'
`main_test.go`, which starts the container in `TestMain`, builds under
`integration || bench`. `-tags=bench` alone therefore builds the
container setup and the benchmarks, and none of the integration tests;
`-tags='bench integration'` builds both. SQLite uses a database file in a
temporary directory, as in production; the memory store needs nothing.

A full run at `-count=6` takes several minutes per backend. Most of it is
seeding (100,000 events for each ReadAll and catch-up run) and SQLite's
catch-up at 16 segments, about 5 s a pass.

## What is measured

The bodies live in two exported helpers, so a backend's benchmark file is a
few lines and a store outside this module can be measured the same way:
`eventsourcingtest.Benchmark` measures an `EventStore`, and
`projectiontest.Benchmark` a projection `Runner` on an event store, a
projection `Store` and a handler that writes a read model. Each reports a custom metric, which compares across
sub-benchmarks where ns/op does not.

| Benchmark | What one operation is | Metric | Why |
|---|---|---|---|
| `EventStore/Append/Streams=1/Writers=1` | one `Save` of one event, to a single stream, with its exact revision | events/s | the uncontended append cost |
| `EventStore/Append/Streams=1000/Writers=32` | the same, from 32 goroutines, each owning every 32nd of 1,000 streams | events/s | what the Postgres append lock changes: appends to different streams no longer commit in parallel |
| `EventStore/LoadStream/Events=N` | reading a whole stream of 10, 100, 1,000 or 10,000 events | ns/op, events/s | the cost of loading an aggregate without a snapshot; the curve that would justify snapshots |
| `EventStore/ReadAll/Limit=N` | paging through 100,000 events of the global log, filtered on event type, at `ReadOptions.Limit` 100, 500 and 2,000 | events/s | the ceiling on how fast any projection can catch up |
| `Projection/CatchUp/Segments=N` | a new projection processing 100,000 events from the start, at 1, 4 and 16 segments, its handler writing a read-model row per event | events/s, reads/event | what segments buy, and what they cost: every segment reads the whole log |
| `Projection/CommandToLive` | `Save` one event, `Runner.Nudge`, then `Runner.WaitUntil` its position, on a projection that is already live | p50-ms, p99-ms | how long a caller waits before its own write shows up in a read model |

Some details:

- Appends state the exact revision, `Revision(0)` for a new stream, as
  `NewCommandHandler` does. Writers never share a stream, so there are no
  conflicts; the benchmark measures the store, not retries.
- The Postgres pool allows 64 connections, so the 32 writers wait on the
  append lock and not on the pool. pgxpool's default is the number of CPUs.
- The projection's handler writes the event to a read model in the
  projection store's database, as a real projection does: it upserts a
  `benchmark_rows` row per stream holding the version applied, through
  `sqlite.Tx` or `postgres.Tx`, so in the transaction that commits the
  checkpoint. The memory backend keeps the rows in a map under a mutex.
  Without a write, catch-up measures reading alone, which is the one cost
  segments multiply.
- Catch-up times only the runner, from `Run` until every segment has
  reached the head; building and stopping the runner are not timed.
  reads/event is the events `ReadAll` returned to all segments together,
  per event in the log.
- CommandToLive nudges the runner after each append, as an application
  that wires appends to `Runner.Nudge` does. Without a nudge, the wait is
  up to `Definition.PollInterval` (1 s by default) longer. The latency
  includes the handler's row write and the projection store's commit of
  the batch, on SQLite and Postgres one transaction.
- The KurrentDB store has no projection store, so it runs only the
  `EventStore` benchmarks. It shares one container across benchmarks, and
  reads the global log after the head it found before seeding.

## Results

Medians of `-count=6`, with benchstat's 95% confidence interval where it
is wider than ±5%. Database containers ran on the same machine as the
benchmarks.

| | |
|---|---|
| Date | 2026-10-22 |
| CPU | AMD Ryzen 7 9700X 8-Core Processor (16 threads), `x86_64` |
| OS | Linux 7.0.0-38-generic |
| Go | go1.26.3 linux/amd64 |
| SQLite | `modernc.org/sqlite`, the version in `sqlite/go.mod`; WAL, `synchronous=NORMAL` |
| Postgres | 16.14 (`postgres:16-alpine`), default configuration |
| KurrentDB | 26.1.1 (`kurrentplatform/kurrentdb:latest`), in-memory database, insecure, single node |
| Docker | 28.4.0 |

### Append throughput (events/s)

| Backend | 1 stream, 1 writer | 1,000 streams, 32 writers |
|---|---:|---:|
| memory | 2.01M ±10% | 1.75M ±18% |
| SQLite | 14.6k ±7% | 12.9k |
| Postgres | 8.64k | 13.7k |
| KurrentDB | 5.2k ±113% | 65.3k |

Under the append lock, 32 Postgres writers still append 1.6 times as fast
as one: encoding, the round trips before the lock and the per-stream
checks overlap, and only the inserts and the commit wait in line.
KurrentDB, which has no such lock, scales 12 times. The single-writer
KurrentDB figure varied widely between runs.

### LoadStream latency (time to read the whole stream)

| Backend | 10 events | 100 events | 1,000 events | 10,000 events |
|---|---:|---:|---:|---:|
| memory | 91 ns | 368 ns | 2.99 µs | 29.2 µs |
| SQLite | 36.2 µs | 286 µs | 2.89 ms | 29.2 ms |
| Postgres | 61.9 µs | 218 µs | 1.73 ms | 18.4 ms ±5% |
| KurrentDB | 139 µs ±38% | 596 µs | 4.54 ms ±6% | 44.2 ms |

Past about 100 events the cost is linear in the stream length: roughly
2 µs an event on Postgres, 3 µs on SQLite and 4.5 µs on KurrentDB.

### ReadAll throughput (events/s)

| Backend | Limit 100 | Limit 500 | Limit 2,000 |
|---|---:|---:|---:|
| memory | 27.6M | 29.9M | 30.2M |
| SQLite | 318k ±6% | 341k | 336k |
| Postgres | 444k | 572k | 615k |
| KurrentDB | 124k ±13% | 197k | 223k |

### Projection catch-up, a row written per event, 100,000 events (events/s)

| Backend | 1 segment | 4 segments | 16 segments |
|---|---:|---:|---:|
| memory | 2.81M ±5% | 3.82M ±5% | 2.83M |
| SQLite | 135k ±6% | 57.8k ±6% | 18.5k ±5% |
| Postgres | 40.7k | 88.8k ±11% | 75.8k ±11% |
| reads/event, every backend | 1 | 4 | 16 |

On Postgres, writing the row is most of the cost: one segment catches up
at 40.7k events/s, a tenth of what it reads. Segments run those writes in
parallel, so 4 segments catch up 2.2 times as fast. Past that the N reads
take over again, and 16 segments are slower than 4. SQLite has one writer:
every segment's transaction waits for the others, so segments only add
reads, and 16 segments catch up at 0.14 times the speed of one. Segment a
projection on Postgres when its handler writes; on SQLite, don't.

### Command to live (ms)

| Backend | p50 | p99 |
|---|---:|---:|
| memory | 0.0022 | 0.0074 ±4% |
| SQLite | 0.10 | 1.29 |
| Postgres | 0.37 | 0.66 ±14% |
