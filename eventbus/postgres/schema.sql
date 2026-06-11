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