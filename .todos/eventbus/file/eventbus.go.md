# TODO: `eventbus/file/eventbus.go`

416 lines. File-backed `EventBus` — one JSON file per subscriber per event, delivered via fsnotify.

## Correctness
- [ ] **Two events dispatched in the same nanosecond overwrite each other.** The filename is
      `fmt.Sprintf("%020d.json", time.Now().UnixNano())` (`:238`), and `os.Rename` replaces silently
      on POSIX. Go's clock resolution is not guaranteed to advance between two adjacent calls — and on
      Windows it is far coarser — so a burst dispatch loses events with no error anywhere. Add a
      monotonic counter or the event ID to the filename.
- [ ] **`_ = os.Rename(tmp, path)`** (`:246`) discards the error. A failed rename leaves a `.tmp` file
      the watcher ignores, so the event is lost silently. This is the last step of the
      write-then-rename that makes delivery atomic — its failure is exactly what must not be dropped.
- [ ] **A per-subscriber write failure is skipped silently** (`continue` on `os.WriteFile` error,
      `:244`). The doc admits it ("a write failure ... is skipped silently and not reported to the
      caller"), but the result is an event that never reaches that subscriber, with nothing on
      `Errors()`. At minimum report it there — the channel exists for exactly this.
- [ ] **`processFile` returns silently when `os.ReadFile` fails** (`:308-311`), while every *decode*
      failure below it is reported via `sendErr`. An unreadable file therefore disappears with no
      signal, and since the file is never removed it is retried forever on each new event.
- [ ] **Delivery order is wall-clock order, not `GlobalVersion` order.** Filenames sort by dispatch
      timestamp, so a backwards clock step (NTP correction) delivers events out of order. If ordering
      matters, name files by `GlobalVersion`.

## Style
- [ ] **Errors don't wrap sentinels** (`:117`, `:129`, `:133`) — `ErrDuplicateHandler` in particular.
      And `"bus is closed"` here vs `"eventbus is closed"` in the memory and postgres buses: three
      implementations, three strings, for one condition.
- [ ] **Stale copy-paste comment** at `:319`: "Wrap and propagate as EventStoreError" — no such type,
      and this is an event *bus*. Same comment appears in `eventstore/file/filestorage.go`.

## Good
- **`context.WithoutCancel(ctx)` for handler invocations** (`:268`), with a comment explaining the
  distinction: cancelling stops the loop from picking up the *next* file, but a handler already
  running completes rather than being cut short by a shutdown racing it. That is the right call and
  the reasoning is written down.
- **Write-to-`.tmp`-then-rename** gives atomic visibility, so a watcher never observes a half-written
  file.
- **A file is deleted only after the handler returns nil**, so a failed handler retries on restart —
  crash recovery falls out of the design rather than being bolted on.
- `ErrSkippedEvent` treated as success (file removed), consistent with the rest of the repo.
- The crash-recovery replay of pre-existing files on `Subscribe` is documented on the type.
