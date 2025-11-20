package eventsourcing

type Revision interface {
	toRawInt64() int64
}

// Any means append without checking current revision.
type Any struct{}

func (Any) toRawInt64() int64 { return -1 } // special marker

// NoStream means the stream should not exist yet.
type NoStream struct{}

func (NoStream) toRawInt64() int64 { return 0 }

// StreamExists means the stream must exist.
type StreamExists struct{}

func (StreamExists) toRawInt64() int64 { return -2 } // special marker

// ExplicitRevision matches exactly a numeric revision.
type ExplicitRevision uint64

func (r ExplicitRevision) toRawInt64() int64 { return int64(r) }
