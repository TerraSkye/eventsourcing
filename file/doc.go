// Package file keeps events as JSON files in a directory: an event store
// and an event bus.
//
// It needs no database and its files can be read with any tool. Only one
// process may write a directory at a time.
//
// Deprecated: use package sqlite, which keeps the log in one file as well
// but with transactions, ordered global positions across processes, and
// exactly-once projections. sqlite.Import copies a file store's events
// into SQLite. Package file will be removed in a later release.
package file
