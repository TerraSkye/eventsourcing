package eventsourcing

// StreamState expresses a caller's expectation of a stream's revision, for
// example to an [EventStore.Save] call via [WithStreamState]. [Any],
// [NoStream], [StreamExists], and [Revision] are the implementations
// recognized by this package's EventStore implementations.
type StreamState interface {
	// ToRawInt64 encodes the expectation as a store-specific raw value: a
	// non-negative [Revision] number, or one of the special negative
	// markers used by [Any] and [StreamExists].
	ToRawInt64() int64
}

// Any expects nothing: append without checking the stream's current
// revision.
type Any struct{}

func (Any) ToRawInt64() int64 { return -1 } // special marker

// NoStream expects the stream not to exist yet.
type NoStream struct{}

func (NoStream) ToRawInt64() int64 { return 0 }

// StreamExists expects the stream to already exist.
type StreamExists struct{}

func (StreamExists) ToRawInt64() int64 { return -2 } // special marker

// Revision expects the stream to be at exactly this version.
type Revision uint64

func (r Revision) ToRawInt64() int64 { return int64(r) }
