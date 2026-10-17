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
    UNIQUE (stream_id, stream_position)
);

CREATE INDEX IF NOT EXISTS idx_events_stream_id ON events (stream_id);

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
    PRIMARY KEY (name, segment)
);

CREATE TABLE IF NOT EXISTS projection_dead_letters (
    id              BIGSERIAL   PRIMARY KEY,
    projection      TEXT        NOT NULL,
    segment         INT         NOT NULL,
    global_position BIGINT      NOT NULL,
    event_type      TEXT        NOT NULL,
    error           TEXT        NOT NULL,
    skipped_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_projection_dead_letters_projection
    ON projection_dead_letters (projection, global_position);
