# dashboard

An `http.Handler` that serves a web page showing how a program's projections and automations are doing, live: how far each segment has processed the log, how far it is behind, how fast it is catching up, and why a stalled segment stalled. It can rebuild a projection, pause and resume automations, and browse the global log.

## Package

```
github.com/terraskye/eventsourcing/dashboard
```

## Constructor

```go
func New(store projection.Store, opts ...Option) *Dashboard
```

Returns a `Dashboard` showing the projections whose status is published to `store`. `*Dashboard` implements `http.Handler`. It serves the page at its root, so mount it on a path ending in a slash; opened without the trailing slash under `http.StripPrefix`, it redirects (301) to the path with one.

```go
d := dashboard.New(projectionStore,
    dashboard.WithRunners(taskList, shipments),
    dashboard.WithProjections("invoices"), // run by another service
)
mux.Handle("/dashboard/", http.StripPrefix("/dashboard", d))
```

The handler has no authentication of its own: wrap it in the program's auth middleware before exposing it. The `examples/dashboard` program in the repository runs one.

## Options

```go
type Option func(*Dashboard)
```

| Option | Default | Description |
|---|---|---|
| `WithRunners(runners ...*projection.Runner)` | | Shows the projections these runners run. Each pushes an update as soon as it commits or changes phase. |
| `WithProjections(names ...string)` | | Shows the projections with these names, as published to the store by whichever program runs them. |
| `WithReset()` | off | Adds a rebuild button to every projection shown, including ones another program runs. |
| `WithAutomations(store automation.Store, names ...string)` | no automations view | Adds an automations view that shows these automations as their instances publish them to `store`, the `automation.Definition.Store` they run with. |
| `WithPause()` | off | Adds a pause and a resume button to every automation shown. Ignored without `WithAutomations`. |
| `WithEvents(reader eventsourcing.GlobalReader)` | no events view | Adds an events view over the global log. If `reader` also implements `eventsourcing.CorrelationLoader`, each event links to its correlation chain. |
| `WithInterval(interval time.Duration)` | `DefaultInterval` = 500ms | How often the dashboard reads the statuses while a page is open. A value of 0 or less keeps the default. |
| `WithClaimTTL(ttl time.Duration)` | `projection.DefaultClaimTTL` | The `projection.Definition.ClaimTTL` the projections run with, so the page can show a segment whose runner stopped publishing when the store does not report the claim's own ttl. |
| `WithAllowedHosts(hosts ...string)` | localhost and IP addresses only | Host names the dashboard answers requests for, compared without the port and case-insensitively. |
| `WithTitle(title string)` | `"Projections"`, or `"Automations"` when only automations are shown | The page's title. |

Names given more than once, or empty, are dropped.

## Routes

All paths are relative to where the handler is mounted.

| Method and path | Answer |
|---|---|
| `GET /` | The page, with `Cache-Control: no-cache` and a Content-Security-Policy that allows inline script and style from the page only. |
| `GET /events` | Server-sent events: one JSON snapshot per read, and a `: keep-alive` comment every 15 seconds. |
| `GET /api/snapshot` | The snapshot as of now, as JSON, for scripts and checks. |
| `POST /api/projections/{name}/rebuild` | Records a rebuild request with `projection.Store.RequestRebuild`. |
| `POST /api/automations/{name}/pause` | Pauses the automation through `automation.Store.SetPaused`. |
| `POST /api/automations/{name}/resume` | Resumes it. |
| `GET /api/events` | A page of the global log, newest first. |
| `GET /api/correlations/{id}` | Every event of one correlation chain, oldest first. |

Every read of a store is bounded by a five-second timeout.

### Host and origin checks

Every request is refused with 403 unless its `Host` is `localhost`, an IP address, or a name given with `WithAllowedHosts`: a site can point a name it controls at the dashboard's address (DNS rebinding), and its page would then pass for the dashboard's own.

The `POST` endpoints additionally accept requests from the dashboard's own page only: `Sec-Fetch-Site` must be `same-origin` when present; otherwise the `Origin` host must equal the request's `Host`; a request with neither header, as from a non-browser client, passes.

### Rebuild responses

| Status | When |
|---|---|
| 202 | The request is recorded. The runner holding segment 0 serves it between two batches; the page shows it as pending until then. |
| 404 | `WithReset` is not set, or `{name}` is not shown on this dashboard. |
| 403 | Not from the dashboard's own page. |
| 409 | The projection runs in this program and has no `Reset`, or no runner has declared it yet (`projection.ErrUnknownProjection`). |
| 500 | The store failed. |

A projection without a `Reset` run by this program has its button hidden. For one run elsewhere, only its runner knows: the request stays pending and that runner logs why.

### Pause and resume responses

| Status | When |
|---|---|
| 200 | Recorded. Every instance stops claiming at its next poll; items already running finish. |
| 404 | `WithPause` is not set, or `{name}` is not shown. |
| 403 | Not from the dashboard's own page. |
| 500 | The store failed. |

### Events query parameters

| Parameter | Default | Description |
|---|---|---|
| `limit` | 50 | Events per page, capped at 500. Must be a positive number. |
| `before` | the head | Return events with a position below this one. |
| `type` | | Event type names; repeatable. Exact match. |
| `stream` | | Stream ID prefixes; repeatable. |

The answer is `{"events": [...], "head": n, "older": n, "scanned": n}`: `older` is the `before` to pass for the next page, or 0 at the start of the log; `scanned` is set when the read stopped after scanning 100,000 positions without filling the page. A `GlobalReader` only reads forwards, so the view reads a window ending at `before` and widens it until it holds `limit` matches. Each event carries `position`, `eventId`, `streamId`, `version`, `type`, `occurredAt`, `correlationId`, `causationId`, `metadata` and `payload` (the event encoded as JSON, or `{"error": ...}` when encoding failed). The view shows payloads to anyone who can open the dashboard.

`GET /api/correlations/{id}` answers 400 for an empty ID (`eventsourcing.ErrEmptyCorrelationID`), 404 when the reader does not implement `CorrelationLoader`, and otherwise `{"correlationId": id, "events": [...]}`, capped at 100,000 events. A store wrapped with `telemetry.WithEventStoreTelemetry` still reads the log but loses `LoadCorrelation`; pass the unwrapped store to `WithEvents`.

## Snapshot

What the page receives on every update, as JSON:

```
title, reset, events, chains, showAutomations
projections[]: name, state, position, head, behind, rate, eta, version, local,
               canReset, rebuildRequested, error, segments[], instances[]
  segments[]:  segment, state, position, head, replayUntil, owner, ageMs,
               lastError, version
  instances[]: owner, version, segments, ageMs
automations[]: name, state, paused, canPause, processed, failed, deferred,
               waitingFor, lastError, error, instances[], segments[]
  instances[]: owner, state, waitingFor, processed, failed, deferred,
               lastError, segments, ageMs
  segments[]:  key, group, owner, pausedMs
```

For a projection, `position` is the lowest segment position, `head` the highest head any segment saw, `behind` the largest backlog of any segment (each against the head it last read itself), `rate` positions per second over the last five seconds (an estimate needs one second of history, and starts over when a rebuild moves the position back), and `eta` seconds to catch up, or 0 when unknown. `instances` includes standby instances holding no segment. An automation's `processed`, `failed` and `deferred` are totals over its live instances.

### Projection states

A projection takes the worst state of its segments, in this order:

| State | Meaning |
|---|---|
| `unreadable` | The store could not be read; `error` has why. |
| `stalled` | A segment's last attempt failed; `lastError` has why. |
| `silent` | A segment has an owner but its status is older than its claim's ttl (the store's `ClaimTTL`, else `WithClaimTTL`) and it is not on standby. |
| `rebuild_requested` | A rebuild waits for segment 0's runner. |
| `not_started` | No runner has declared the projection. |
| `waiting` | A segment is starting or unowned. |
| `replaying` | A segment is catching up below its `ReplayUntil`. |
| `catching_up` | A segment is behind the head of the log. |
| `live` | Every segment is at the head of the log. |

### Automation states

| State | Meaning |
|---|---|
| `unreadable` | The store could not be read. |
| `stalled` | An instance's last check or scheduler call failed. |
| `paused` | Paused through the store. A pause outranks every instance's state. |
| `not_started` | No instance has published a status. |
| `waiting` | An instance is starting or waiting for a dependency. |
| `working` | An instance is working claimed items. |
| `idle` | The instances found nothing to do. |

## Updates

While at least one page is open, the dashboard reads every projection's statuses and instances, and every automation's pause flag, statuses and segments, once per interval, however many pages are open, and stops reading when the last page closes. A runner given with `WithRunners` also triggers a read as soon as it commits or changes phase; reads are at least 100ms apart. A page that opens receives the latest snapshot at once.

## Related

- [projection reference](./projection.md) for the phases and statuses the page is built from
- [automation reference](./automation.md) for `Store.SetPaused` and the automation phases
- [Rebuild a projection without downtime](../how-to/rebuild-without-downtime.md)
