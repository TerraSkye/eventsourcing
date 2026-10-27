// Package kurrentdb keeps events in KurrentDB (formerly EventStoreDB).
//
// [EventStore] appends to and reads KurrentDB streams, and reads the $all
// stream for [eventsourcing.GlobalReader], so a projection.Runner can
// consume them.
package kurrentdb
