package sqlite

import (
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
// Schema has the tables as this release uses them, and only creates what
// is missing: it adds no column to a table an earlier release created.
// The changelog lists the ALTER TABLE statements to run when upgrading.
//
//go:embed schema.sql
var Schema string

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
