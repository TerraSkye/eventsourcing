// Package kurrentdb keeps events in KurrentDB (formerly EventStoreDB): an
// event store and an event bus on one client.
//
// [EventStore] appends to and reads KurrentDB streams, and reads the $all
// stream for [eventsourcing.GlobalReader]. [EventBus] delivers events to
// subscribers through persistent subscriptions, so a subscriber resumes
// where it left off after a restart.
package kurrentdb
