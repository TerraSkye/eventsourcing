// Package memory keeps everything in process memory: an event store and a
// projection store.
//
// It is meant for tests, examples and single-process programs whose state
// may be lost on restart. Nothing survives the process, and the projection
// store cannot roll back a handler's writes; see [ProjectionStore].
package memory
