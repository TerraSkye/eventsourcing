package eventsourcing

import (
	"errors"
	"fmt"
)

var (
	ErrStreamNotFound  = errors.New("stream not found")
	ErrHandlerNotFound = errors.New("handler not registered")
)

type StreamRevisionConflictError struct {
	Stream           string
	ExpectedRevision uint64
	ActualRevision   uint64
}

func (s StreamRevisionConflictError) Error() string {
	return "stream revision conflict"
}

// ErrSkippedEvent is returned when a handler cannot handle the event type.
type ErrSkippedEvent struct {
	Event Event
}

func (e ErrSkippedEvent) Error() string {
	return fmt.Sprintf("skipped event of type %T", e.Event)
}
