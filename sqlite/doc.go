// Package sqlite keeps events, subscriptions and projections in SQLite: an
// event store, an event bus and a projection store that share one
// database file, or one in-memory database.
//
// It needs no server, which suits local development, tutorials and tests
// that need real transactions, and it serves single-process production
// services as well (WAL mode, replicated with a tool such as Litestream).
// Use Postgres or KurrentDB when several processes share the log.
//
// Open a database with [Open] and apply the schema with [ApplySchema],
// which also upgrades a database an earlier release created:
//
//	db, err := sqlite.Open("events.db") // or ":memory:"
//	if err != nil {
//		return err
//	}
//	if err := sqlite.ApplySchema(ctx, db); err != nil {
//		return err
//	}
//	store := sqlite.NewEventStore(db)
//
// The driver is modernc.org/sqlite, which is pure Go, so no C toolchain is
// needed.
//
// # One connection
//
// [Open] limits the database to one connection, so writers never meet
// SQLITE_BUSY and an in-memory database is one database rather than one
// per connection. Every call therefore runs alone, which has one
// consequence for projections: a handler runs inside the projection
// store's transaction and must write through [Tx]. A handler that uses the
// *sql.DB directly waits for a connection that its own batch holds, and
// never returns.
package sqlite
