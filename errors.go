package eventsourcing

import "fmt"

type StreamRevisionConflictError struct {
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

type EventStoreError struct {
	Err error
}

func (e *EventStoreError) Error() string {
	return fmt.Sprintf("eventstore error: %v", e.Err)
}

func (e *EventStoreError) Unwrap() error {
	return e.Err
}

func WrapEventStoreError(err error) error {
	if err == nil {
		return nil
	}
	return &EventStoreError{Err: err}
}
