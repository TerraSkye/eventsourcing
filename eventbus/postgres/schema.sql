CREATE TABLE IF NOT EXISTS event_subscriptions (
    name     VARCHAR PRIMARY KEY,
    position BIGINT  NOT NULL DEFAULT 0
);