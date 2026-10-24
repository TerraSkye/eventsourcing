package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/terraskye/eventsourcing/sqlite"
)

// schemaV020 is the events and projection_segments tables as v0.2.0
// created them, before correlation_id, causation_id and ttl_ms.
const schemaV020 = `
CREATE TABLE events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id        TEXT    NOT NULL,
    stream_id       TEXT    NOT NULL,
    stream_position INTEGER NOT NULL,
    event_type      TEXT    NOT NULL,
    payload         BLOB    NOT NULL,
    metadata        BLOB,
    occurred_at     INTEGER NOT NULL,
    UNIQUE (stream_id, stream_position)
);
CREATE INDEX idx_events_event_type ON events (event_type, id);
CREATE TABLE projection_segments (
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
    PRIMARY KEY (name, segment)
);
INSERT INTO events (event_id, stream_id, stream_position, event_type, payload, occurred_at)
VALUES ('e1', 's1', 1, 'TaskCreated', x'7b7d', 0);
INSERT INTO projection_segments (name, segment) VALUES ('tasks', 0);
`

// TestApplySchema_UpgradesV020 covers a database v0.2.0 created: Schema
// alone fails on it, ApplySchema adds the missing columns with their
// defaults and keeps the rows, and applying it again changes nothing.
func TestApplySchema_UpgradesV020(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(t.Context(), schemaV020); err != nil {
		t.Fatalf("create the v0.2.0 tables: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), sqlite.Schema); err == nil {
		t.Fatal("Schema on a v0.2.0 database succeeded, want it to fail on the missing correlation_id")
	}

	for range 2 {
		if err := sqlite.ApplySchema(t.Context(), db); err != nil {
			t.Fatalf("ApplySchema: %v", err)
		}
	}
	var correlation, causation string
	if err := db.QueryRowContext(t.Context(),
		`SELECT correlation_id, causation_id FROM events WHERE event_id = 'e1'`,
	).Scan(&correlation, &causation); err != nil || correlation != "" || causation != "" {
		t.Fatalf("event after ApplySchema: correlation %q, causation %q, %v; want both empty", correlation, causation, err)
	}
	var ttl int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT ttl_ms FROM projection_segments WHERE name = 'tasks'`,
	).Scan(&ttl); err != nil || ttl != 15000 {
		t.Fatalf("segment ttl_ms after ApplySchema = %d, %v; want 15000", ttl, err)
	}
	assertTable(t, db, "projection_replays")
}

// TestApplySchema_NewDatabase covers an empty database: ApplySchema
// creates every table, as Schema does.
func TestApplySchema_NewDatabase(t *testing.T) {
	db, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlite.ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	for _, table := range []string{"events", "event_subscriptions", "projection_segments", "projection_instances", "projection_replays"} {
		assertTable(t, db, table)
	}
}

func assertTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&n); err != nil || n != 1 {
		t.Fatalf("table %s after ApplySchema: %d, %v; want it created", table, n, err)
	}
}
