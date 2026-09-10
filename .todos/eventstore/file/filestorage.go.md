# TODO: `eventstore/file/filestorage.go`

547 lines. File-backed `EventStore` — one JSON file per event, plus an `all/` symlink fan-in.

## Correctness
- [ ] **Events silently overwrite each other when `Version` is unset — already documented in the code's
      own TODO (`:203-209`).** Each file is named after `events[i].Version`, but `Save` never assigns
      that field, unlike the postgres and kurrentdb stores which compute the position server-side.
      A batch left at the zero value collapses to one file and `Save` still reports success:
      *"saving 3 events with Version left unset leaves only 1 file, and LoadStream then returns only
      that 1 event"*. Silent data loss on the happy path. Assign `Version` from `currentVersion`, the
      way the other stores do.
- [ ] **Cross-process writers can overwrite each other's stream files.** `currentVersion` comes from
      a `ReadDir` count taken under `f.mu` — a *process-local* lock. Two `FilesStore` instances on one
      directory can both compute `N` and both write `N+1`. The symlink collision in `all/` catches a
      *global* version clash, but nothing catches the per-stream one. The type doc claims "safe for
      concurrent use"; scope that claim to a single process.
- [ ] **Four ignored errors on the write path:**
      - `os.MkdirAll(sdir, 0o755)` (`:224`) — an unwritable directory is discovered later, as a
        confusing marshal/write failure.
      - `files, _ := os.ReadDir(sdir)` (`:227`) — an unreadable stream directory yields
        `currentVersion = 0`, so a `NoStream` assertion **passes** for a stream that exists.
      - `rel, _ := filepath.Rel(...)` (`:311`) — a bad relative path produces a dangling symlink.
      - `case _, ok := <-f.watcher.Errors` (`:490`) — watcher errors are read and thrown away, so the
        global-sequence sync can silently stop working.
- [ ] **Three ignored errors on the read path** (`loadFromDir`): a file that fails `os.ReadFile` or
      `json.Unmarshal` is `continue`d past (`:468`, `:474`), so a corrupt event is silently omitted
      from the stream — the aggregate rebuilds from incomplete history and reports success. This is
      the worst failure mode an event store can have. Compare the same function's `NewEventByName`
      and payload-unmarshal failures, which correctly return an error.
- [ ] **`currentVersion := uint64(len(files))` counts directory entries**, so any stray file — an
      editor swap file, `.DS_Store` — inflates the stream version and corrupts the concurrency check.
      Count only entries matching the event filename pattern (`parseGlobalVersion` already exists).
- [ ] **`ExpectedRevision: revision` in the symlink-collision conflict** (`:322`) reports the caller's
      `StreamState`, which may be `Any{}` — formatting it renders "expected version -1". Same issue as
      the postgres store.

## Style
- [ ] **`Save` holds `f.mu` across all disk I/O for the whole batch**, serialising every write to every
      stream behind one mutex. Postgres locks per stream. Acceptable for a test/dev store, but say so.
- [ ] **Stale copy-paste comment** at `:477`: "Convert KurrentDB event to cqrs.EventData" — wrong
      store, and `EventData` is not a type in this codebase.
- [ ] **`json.Unmarshal(storedEv.Data, &ev)`** (`:483`) takes the address of an interface, while the
      postgres store passes the interface directly. Both work; pick one.
- [ ] `var streamID = events[0].StreamID` (`:211`) — `:=`.

## Good
- **The rollback design is genuinely careful.** `written` accumulates every path in creation order and
  undoes exactly what this call made, and the comment at `:262-267` explains why `globalSeq` is
  deliberately *not* rolled back with it — those versions were briefly visible to a peer's watcher,
  so reissuing them would recreate the conflict. That is a subtle, correct decision, written down.
- **Publishing to `Events` only after the whole batch is durable** (`:337-339`), so a subscriber never
  sees part of a batch that later rolled back.
- **`streamsDirName` exists specifically so a stream literally named `"all"` cannot collide with
  `allDirName`** — and the constant's doc says exactly that.
- **`watchGlobalSequence` is attached before the recovery listing**, with a comment explaining that
  ordering prevents a race with a concurrent writer.
- **`Close` waits on `watchDone` and explains why `f.mu` must not be held across the wait** — the kind
  of deadlock that is otherwise found in production.
- `%010d` zero-padding makes lexical `ReadDir` order match numeric order; `loadFromDir`'s `oneIndexed`
  parameter documents the 0-based vs 1-based split between stream and global numbering.
