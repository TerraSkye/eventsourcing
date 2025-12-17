package eventsourcing

type StreamState interface {
	ToRawInt64() int64
}

// Any means append without checking current revision.
type Any struct{}

func (Any) ToRawInt64() int64 { return -1 } // special marker

// NoStream means the stream should not exist yet.
type NoStream struct{}

func (NoStream) ToRawInt64() int64 { return 0 }

// StreamExists means the stream must exist.
type StreamExists struct{}

func (StreamExists) ToRawInt64() int64 { return -2 } // special marker

// Revision matches exactly a numeric revision.
type Revision uint64

func (r Revision) ToRawInt64() int64 { return int64(r) }
