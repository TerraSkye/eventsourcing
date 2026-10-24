package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Schema creates every table the package uses: the events table of
// [EventStore], the subscriptions table of [EventBus], and the segment and
// dead-letter tables of [ProjectionStore]. It is idempotent; run it with
// your migrations, or once at startup. The same SQL ships as schema.sql in
// this package's directory.
//
// Schema only creates what is missing, so it does not add a column a
// later release added to an existing table. [ApplySchema] does both.
//
//go:embed schema.sql
var Schema string

// addedColumns are the columns added to a table after it shipped, which
// Schema's CREATE TABLE IF NOT EXISTS does not add to an existing table.
var addedColumns = []struct{ table, column, definition string }{
	{"events", "correlation_id", "TEXT NOT NULL DEFAULT ''"},            // v0.3.0
	{"events", "causation_id", "TEXT NOT NULL DEFAULT ''"},              // v0.3.0
	{"projection_segments", "ttl_ms", "INTEGER NOT NULL DEFAULT 15000"}, // v0.4.0
}

// ApplySchema brings db up to the current [Schema], in one transaction. It
// adds the columns that later releases added to tables db already has,
// then applies Schema, which creates the tables and indexes db lacks. Run
// it once at startup instead of executing Schema, on a new database or one
// created by an earlier release; on a current one it changes nothing.
func ApplySchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	defer tx.Rollback()
	for _, c := range addedColumns {
		var exists, missing bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?),
			       NOT EXISTS (SELECT 1 FROM pragma_table_info(?) WHERE name = ?)`,
			c.table, c.table, c.column,
		).Scan(&exists, &missing); err != nil {
			return fmt.Errorf("sqlite: apply schema: inspect %s: %w", c.table, err)
		}
		if !exists || !missing {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE "+c.table+" ADD COLUMN "+c.column+" "+c.definition); err != nil {
			return fmt.Errorf("sqlite: apply schema: add %s.%s: %w", c.table, c.column, err)
		}
	}
	if _, err := tx.ExecContext(ctx, Schema); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return nil
}

// Open opens the SQLite database at path, creating the file if it does not
// exist; ":memory:" opens a database that lives as long as the returned
// *sql.DB. It limits the pool to one connection (see the package docs),
// turns on foreign keys, and for a file uses WAL journaling, begins every
// transaction with the write lock, and waits up to five seconds for a lock
// held by another process.
func Open(path string) (*sql.DB, error) {
	pragmas := []string{"foreign_keys(1)"}
	if path != ":memory:" {
		pragmas = append(pragmas, "journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)")
	}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	if path != ":memory:" {
		// Take the write lock when a transaction begins, not at its first
		// write, so a read-check-write such as Save's revision check cannot
		// be overtaken by another process in between.
		q.Set("_txlock", "immediate")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	// Keep the one connection: closing it would drop an in-memory database.
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite: open %q: %w", path, err)
	}
	return db, nil
}
