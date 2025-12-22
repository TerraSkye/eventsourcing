package eventsourcing

import (
	"errors"
	"fmt"
)

var (
	ErrStreamNotFound        = errors.New("stream not found")
	ErrStreamExists          = errors.New("stream already exists")
	ErrInvalidEventBatch     = errors.New("invalid event batch")
	ErrHandlerNotFound       = errors.New("handler not registered")
	ErrInvalidRevision       = errors.New("invalid revision")
	ErrHandlerNotRegistered  = errors.New("no handler registered for type")
	ErrDuplicateHandler      = errors.New("duplicate handler registered ")
	ErrHandlerPanicked       = errors.New("handler panicked when handling command")
	ErrCommandBusClosed      = errors.New("command bus is closed")
	ErrBusinessRuleViolation = errors.New("business rule violation")
)

type StreamRevisionConflictError struct {
	Stream           string
	ExpectedRevision StreamState
	ActualRevision   StreamState
}

func (s StreamRevisionConflictError) Error() string {
	return fmt.Sprintf("concurrency conflict on stream %q: (expected version %d, actual %d)",
		s.Stream, s.ExpectedRevision, s.ActualRevision,
	)
}

// ErrSkippedEvent is returned when a handler cannot handle the event type.
type ErrSkippedEvent struct {
	Event Event
}

func (e ErrSkippedEvent) Error() string {
	return fmt.Sprintf("skipped event of type %T", e.Event)
}
