package eventsourcing

import (
	"errors"
	"fmt"
)

var (
	// ErrStreamNotFound is returned by an [EventStore] when [StreamExists] is
	// required for a stream that does not exist.
	ErrStreamNotFound = errors.New("stream not found")
	// ErrStreamExists is returned by an [EventStore] when [NoStream] is
	// required for a stream that already exists.
	ErrStreamExists = errors.New("stream already exists")
	// ErrInvalidEventBatch is returned by [EventStore.Save] when the given
	// envelopes do not all share the same stream ID.
	ErrInvalidEventBatch = errors.New("invalid event batch")
	// ErrHandlerNotFound is returned by a [QueryBus] when no [QueryHandler]
	// is registered for a query's type.
	ErrHandlerNotFound = errors.New("handler not registered")
	// ErrInvalidRevision is returned by an [EventStore] when given a
	// [StreamState] implementation it does not support.
	ErrInvalidRevision = errors.New("invalid revision")
	// ErrHandlerNotRegistered is returned by a [CommandBus] when no
	// [CommandHandler] is registered for a command's type.
	ErrHandlerNotRegistered = errors.New("no handler registered for type")
	// ErrDuplicateHandler is returned or panicked with when registering a
	// second handler for a command or event type that already has one, by
	// [CommandBus.Register], [EventBus.Subscribe] implementations, and
	// [QueryBus] registration.
	ErrDuplicateHandler = errors.New("duplicate handler registered ")
	// ErrHandlerPanicked is joined into the error a [CommandBus] returns
	// when a [CommandHandler] panics instead of returning an error.
	ErrHandlerPanicked = errors.New("handler panicked when handling command")
	// ErrCommandBusClosed is wrapped into the error a [CommandBus] returns
	// when Dispatch is called after Stop.
	ErrCommandBusClosed = errors.New("command bus is closed")
	// ErrEventNotRegistered is returned by [NewEventByName] when no event
	// type is registered under the given name.
	ErrEventNotRegistered = errors.New("event not registered")
)

// StreamRevisionConflictError is returned by an [EventStore] when a save is
// made against a [Revision] that no longer matches the stream's actual
// revision — an optimistic-concurrency conflict.
type StreamRevisionConflictError struct {
	Stream           string
	ExpectedRevision StreamState
	ActualRevision   StreamState
}

func (s StreamRevisionConflictError) Error() string {
	return fmt.Sprintf("concurrency conflict on stream %q: (expected version %d, actual %d)",
		s.Stream, s.ExpectedRevision.ToRawInt64(), s.ActualRevision.ToRawInt64(),
	)
}

// ErrSkippedEvent is returned when an [EventHandler] declines to process an
// [Event] because it is not the type the handler expects.
type ErrSkippedEvent struct {
	Event Event
}

func (e ErrSkippedEvent) Error() string {
	return fmt.Sprintf("skipped event of type %T", e.Event)
}

// ErrBusinessRuleViolation wraps an error returned when a [Command] violates
// a business rule. Use it to signal expected, recoverable domain-level
// rejections, as opposed to infrastructure or persistence errors. Its cause
// is unexported — construct one with [NewBusinessRuleViolation] and read the
// cause back with [ErrBusinessRuleViolation.Cause] or [errors.Unwrap].
type ErrBusinessRuleViolation struct {
	err error
}

// NewBusinessRuleViolation wraps err as an [ErrBusinessRuleViolation]. If err
// is nil, it returns nil, so a decide function can pass a possibly-nil
// validation error straight through without an explicit nil check:
//
//	func decide(state State, cmd Command) ([]Event, error) {
//		return nil, NewBusinessRuleViolation(validate(state, cmd))
//	}
func NewBusinessRuleViolation(err error) error {
	if err == nil {
		return nil
	}
	return &ErrBusinessRuleViolation{err: err}
}

func (e ErrBusinessRuleViolation) Error() string {
	if e.err == nil {
		return "business rule violation"
	}
	return fmt.Sprintf("business rule violation :%s", e.err.Error())
}

func (e ErrBusinessRuleViolation) Cause() error {
	return e.err
}

func (e ErrBusinessRuleViolation) Unwrap() error {
	return e.err
}
