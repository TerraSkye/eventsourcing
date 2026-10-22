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
	ErrHandlerNotRegistered = errors.New("no handler registered")
	// ErrDuplicateHandler is returned or panicked with when registering a
	// second handler for a command or event type that already has one, by
	// [CommandBus.Register], [EventBus.Subscribe] implementations, and
	// [QueryBus] registration.
	ErrDuplicateHandler = errors.New("duplicate handler registered")
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

// Error formats the conflict through each [StreamState]'s own String method,
// so a store that reports a conflict without knowing the revisions involved —
// as the KurrentDB one does — renders them as <nil> instead of panicking.
func (s *StreamRevisionConflictError) Error() string {
	return fmt.Sprintf("concurrency conflict on stream %q: (expected version %v, actual %v)",
		s.Stream, s.ExpectedRevision, s.ActualRevision,
	)
}

// SkippedEventError is returned when an [EventHandler] declines to process an
// [Event]: by a handler built with [OnEvent] when the event is not its type,
// and by an [EventGroupProcessor] when it has no handler for the event's
// type.
type SkippedEventError struct {
	// Event is the event that was skipped.
	Event Event
	// Expected is the type the handler processes, such as "*orders.Placed",
	// or "" when it processes several, as an EventGroupProcessor does.
	Expected string
	// Envelope is the envelope the event arrived in, as [WithEnvelope] put
	// it on the handler's ctx, or nil when ctx carries none.
	Envelope *Envelope
}

func (e *SkippedEventError) Error() string {
	msg := fmt.Sprintf("skipped event of type %T", e.Event)
	if e.Expected != "" {
		msg += fmt.Sprintf(", handler processes %s", e.Expected)
	}
	if e.Envelope != nil {
		msg += fmt.Sprintf(" (stream %q, version %d)", e.Envelope.StreamID, e.Envelope.Version)
	}
	return msg
}

// BusinessRuleViolationError wraps an error returned when a [Command] violates
// a business rule. Use it to signal expected, recoverable domain-level
// rejections, as opposed to infrastructure or persistence errors. Its cause
// is unexported — construct one with [NewBusinessRuleViolation] and read the
// cause back with [BusinessRuleViolationError.Cause] or [errors.Unwrap].
type BusinessRuleViolationError struct {
	err error
}

// NewBusinessRuleViolation wraps err as a [BusinessRuleViolationError]. If err
// is nil, it returns nil.
//
// [NewCommandHandler] already wraps whatever error its [Decider] returns in
// a BusinessRuleViolationError, so a Decider used with it should just return
// the plain error — calling this in a Decider would nest one violation
// inside another. Use it when signaling a business rule violation from a
// hand-rolled [CommandHandler] that doesn't go through NewCommandHandler's
// decide step, so callers (e.g. an OpenTelemetry middleware) can still
// recognize it as an expected, recoverable rejection via [errors.As].
func NewBusinessRuleViolation(err error) error {
	if err == nil {
		return nil
	}
	return &BusinessRuleViolationError{err: err}
}

func (e *BusinessRuleViolationError) Error() string {
	if e.err == nil {
		return "business rule violation"
	}
	return fmt.Sprintf("business rule violation: %s", e.err.Error())
}

func (e *BusinessRuleViolationError) Cause() error {
	return e.err
}

func (e *BusinessRuleViolationError) Unwrap() error {
	return e.err
}
