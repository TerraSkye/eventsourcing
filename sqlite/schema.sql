-- Schema for every table the sqlite package uses. It is idempotent: apply
-- it with your migrations, or once at startup through sqlite.Schema.

-- Event store: the global log. id is the global position. AUTOINCREMENT
-- keeps an id from being reused after the newest event is deleted.
CREATE TABLE IF NOT EXISTS events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        TEXT    NOT NULL,
    stream_id       TEXT    NOT NULL,
    stream_position INTEGER NOT NULL,
    event_type      TEXT    NOT NULL,
    payload         BLOB    NOT NULL,
    metadata        BLOB,
    occurred_at     INTEGER NOT NULL, -- Unix time in nanoseconds, UTC
    correlation_id  TEXT    NOT NULL DEFAULT '',
    causation_id    TEXT    NOT NULL DEFAULT '',
    UNIQUE (stream_id, stream_position)
);

CREATE INDEX IF NOT EXISTS idx_events_event_type ON events (event_type, id);
CREATE INDEX IF NOT EXISTS idx_events_correlation_id ON events (correlation_id);

-- Event bus: the position each subscription has delivered up to.
CREATE TABLE IF NOT EXISTS event_subscriptions (
    name     TEXT    PRIMARY KEY,
    position INTEGER NOT NULL DEFAULT 0
);

-- Projections: one row per segment of each projection (checkpoint, claim
-- with epoch and heartbeat, status), and the events skipped under a
-- non-stalling error policy. Heartbeats are Unix time in milliseconds.
CREATE TABLE IF NOT EXISTS projection_segments (
    name         TEXT    NOT NULL,
    segment      INTEGER NOT NULL,
    position     INTEGER NOT NULL DEFAULT 0,
    version      INTEGER NOT NULL DEFAULT 0,
    replay_until INTEGER NOT NULL DEFAULT 0,
    fingerprint  TEXT    NOT NULL DEFAULT '',
    phase        TEXT    NOT NULL DEFAULT 'starting',
    head         INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT,
    owner        TEXT,
    epoch        INTEGER NOT NULL DEFAULT 0,
    heartbeat    INTEGER NOT NULL DEFAULT 0,
    ttl_ms       INTEGER NOT NULL DEFAULT 15000, -- the claim's own ttl
    PRIMARY KEY (name, segment)
);

-- Rebuild requests per projection: requested counts RequestRebuild calls,
-- done the ones a Reset has served, and failed the last one a rebuild
-- failed to serve, with its error.
CREATE TABLE IF NOT EXISTS projection_rebuilds (
    name      TEXT    PRIMARY KEY,
    requested INTEGER NOT NULL DEFAULT 0,
    done      INTEGER NOT NULL DEFAULT 0,
    failed    INTEGER NOT NULL DEFAULT 0,
    error     TEXT
);

-- The live instances of each projection, to spread its segments over:
-- one row per instance, refreshed by its heartbeat (Unix time in
-- milliseconds).
CREATE TABLE IF NOT EXISTS projection_instances (
    name      TEXT    NOT NULL,
    owner     TEXT    NOT NULL,
    version   INTEGER NOT NULL,
    heartbeat INTEGER NOT NULL,
    ttl_ms    INTEGER NOT NULL,
    PRIMARY KEY (name, owner)
);

CREATE TABLE IF NOT EXISTS projection_dead_letters (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    projection      TEXT    NOT NULL,
    segment         INTEGER NOT NULL,
    global_position INTEGER NOT NULL,
    stream_id       TEXT    NOT NULL DEFAULT '',
    event_type      TEXT    NOT NULL,
    error           TEXT    NOT NULL,
    skipped_at      INTEGER NOT NULL -- Unix time in milliseconds
);

CREATE INDEX IF NOT EXISTS idx_projection_dead_letters_projection
    ON projection_dead_letters (projection, segment, global_position);

-- Requests to apply a stream's dead letters again (Runner.ReplayStream),
-- and per segment holding dead letters of the stream, whether it tried the
-- request and the error it got.
CREATE TABLE IF NOT EXISTS projection_replays (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    name      TEXT    NOT NULL,
    stream_id TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_projection_replays_name ON projection_replays (name);

CREATE TABLE IF NOT EXISTS projection_replay_segments (
    replay  INTEGER NOT NULL REFERENCES projection_replays (id) ON DELETE CASCADE,
    segment INTEGER NOT NULL,
    done    INTEGER NOT NULL DEFAULT 0,
    error   TEXT,
    PRIMARY KEY (replay, segment)
);
