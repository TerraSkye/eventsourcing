-- Schema for every table the postgres package uses. It is idempotent: apply
-- it with your migrations, or once at startup through postgres.Schema.

-- Event store: the global log. id is the global position.
CREATE TABLE IF NOT EXISTS events (
    id              BIGSERIAL   PRIMARY KEY,
    event_id        UUID        NOT NULL,
    stream_id       VARCHAR     NOT NULL,
    stream_position BIGINT      NOT NULL,
    event_type      VARCHAR     NOT NULL,
    payload         BYTEA       NOT NULL,
    metadata        BYTEA,
    occurred_at     TIMESTAMPTZ NOT NULL,
    correlation_id  TEXT        NOT NULL DEFAULT '',
    causation_id    TEXT        NOT NULL DEFAULT '',
    UNIQUE (stream_id, stream_position)
);

-- Added after v0.2.0; adds the chain columns to an existing events table.
ALTER TABLE events ADD COLUMN IF NOT EXISTS correlation_id TEXT NOT NULL DEFAULT '';
ALTER TABLE events ADD COLUMN IF NOT EXISTS causation_id   TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_events_stream_id ON events (stream_id);
CREATE INDEX IF NOT EXISTS idx_events_correlation_id ON events (correlation_id);

-- Event bus: subscription positions, and a NOTIFY after every insert so
-- subscribers wake up without waiting for their poll.
CREATE TABLE IF NOT EXISTS event_subscriptions (
    name     VARCHAR PRIMARY KEY,
    position BIGINT  NOT NULL DEFAULT 0
);

CREATE OR REPLACE FUNCTION eventsourcing_notify_events_inserted() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('eventsourcing_events_inserted', '');
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS eventsourcing_events_notify ON events;
CREATE TRIGGER eventsourcing_events_notify
    AFTER INSERT ON events
    FOR EACH STATEMENT
    EXECUTE FUNCTION eventsourcing_notify_events_inserted();

-- Projections: one row per segment of each projection (checkpoint, claim
-- with epoch and heartbeat, status), and the events skipped under a
-- non-stalling error policy.
CREATE TABLE IF NOT EXISTS projection_segments (
    name         TEXT        NOT NULL,
    segment      INT         NOT NULL,
    position     BIGINT      NOT NULL DEFAULT 0,
    version      INT         NOT NULL DEFAULT 0,
    replay_until BIGINT      NOT NULL DEFAULT 0,
    fingerprint  TEXT        NOT NULL DEFAULT '',
    phase        TEXT        NOT NULL DEFAULT 'starting',
    head         BIGINT      NOT NULL DEFAULT 0,
    last_error   TEXT,
    owner        TEXT,
    epoch        BIGINT      NOT NULL DEFAULT 0,
    heartbeat    TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_ms       BIGINT      NOT NULL DEFAULT 15000,
    PRIMARY KEY (name, segment)
);

-- The claim's own ttl, so every instance expires a claim alike; 15s, the
-- default ClaimTTL, for rows claimed before the column existed.
ALTER TABLE projection_segments ADD COLUMN IF NOT EXISTS ttl_ms BIGINT NOT NULL DEFAULT 15000;

-- Rebuild requests per projection: requested counts RequestRebuild calls,
-- done the ones a Reset has served.
CREATE TABLE IF NOT EXISTS projection_rebuilds (
    name      TEXT   PRIMARY KEY,
    requested BIGINT NOT NULL DEFAULT 0,
    done      BIGINT NOT NULL DEFAULT 0
);

-- The live instances of each projection, to spread its segments over:
-- one row per instance, refreshed by its heartbeat.
CREATE TABLE IF NOT EXISTS projection_instances (
    name      TEXT        NOT NULL,
    owner     TEXT        NOT NULL,
    version   INT         NOT NULL,
    heartbeat TIMESTAMPTZ NOT NULL DEFAULT now(),
    ttl_ms    BIGINT      NOT NULL,
    PRIMARY KEY (name, owner)
);

CREATE TABLE IF NOT EXISTS projection_dead_letters (
    id              BIGSERIAL   PRIMARY KEY,
    projection      TEXT        NOT NULL,
    segment         INT         NOT NULL,
    global_position BIGINT      NOT NULL,
    stream_id       TEXT        NOT NULL DEFAULT '',
    event_type      TEXT        NOT NULL,
    error           TEXT        NOT NULL,
    skipped_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_projection_dead_letters_projection
    ON projection_dead_letters (projection, segment, global_position);

-- Requests to apply a stream's dead letters again (Runner.ReplayStream),
-- and per segment holding dead letters of the stream, whether it tried the
-- request and the error it got.
CREATE TABLE IF NOT EXISTS projection_replays (
    id        BIGSERIAL PRIMARY KEY,
    name      TEXT      NOT NULL,
    stream_id TEXT      NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_projection_replays_name ON projection_replays (name);

CREATE TABLE IF NOT EXISTS projection_replay_segments (
    replay  BIGINT  NOT NULL REFERENCES projection_replays (id) ON DELETE CASCADE,
    segment INT     NOT NULL,
    done    BOOLEAN NOT NULL DEFAULT false,
    error   TEXT,
    PRIMARY KEY (replay, segment)
);
