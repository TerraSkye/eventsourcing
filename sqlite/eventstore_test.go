package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/sqlite"
)

// databases lists the two ways the package is used: in memory, as in
// tests, and in a file, as in development and single-process production.
var databases = []struct {
	name string
	path func(t *testing.T) string
}{
	{"Memory", func(*testing.T) string { return ":memory:" }},
	{"File", func(t *testing.T) string { return filepath.Join(t.TempDir(), "events.db") }},
}

// openDB opens a database at path with the schema applied, closed when the
// test ends.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.ExecContext(t.Context(), sqlite.Schema); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	return db
}

func TestAcceptance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			eventsourcingtest.AcceptanceTest(t, sqlite.NewEventStore(openDB(t, d.path(t))))
		})
	}
}

func TestGlobalReaderAcceptance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			store := sqlite.NewEventStore(openDB(t, d.path(t)))
			eventsourcingtest.GlobalReaderAcceptanceTest(t, store, store)
		})
	}
}

func TestCorrelationLoaderAcceptance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			store := sqlite.NewEventStore(openDB(t, d.path(t)))
			eventsourcingtest.CorrelationLoaderAcceptanceTest(t, store, store)
		})
	}
}

func TestOpen_SchemaIsIdempotent(t *testing.T) {
	db := openDB(t, ":memory:")
	if _, err := db.ExecContext(t.Context(), sqlite.Schema); err != nil {
		t.Fatalf("applying the schema a second time: %v", err)
	}
}
