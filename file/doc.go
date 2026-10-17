// Package file keeps events as JSON files in a directory: an event store
// and an event bus.
//
// It needs no database and its files can be read with any tool, which
// suits local development and tutorials. Only one process may write a
// directory at a time.
package file
