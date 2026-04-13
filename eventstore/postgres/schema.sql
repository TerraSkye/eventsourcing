CREATE TABLE IF NOT EXISTS events (
    id             BIGSERIAL    PRIMARY KEY,
    event_id       UUID         NOT NULL,
    stream_id      VARCHAR      NOT NULL,
    stream_position BIGINT      NOT NULL,
    event_type     VARCHAR      NOT NULL,
    payload        BYTEA        NOT NULL,
    metadata       BYTEA,
    occurred_at    TIMESTAMPTZ  NOT NULL,
    UNIQUE (stream_id, stream_position)
);

CREATE INDEX IF NOT EXISTS idx_events_stream_id ON events (stream_id);