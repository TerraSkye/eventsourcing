package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

// StreamNamer produces the stream name for a given command, with access to context
type StreamNamer func(ctx context.Context, cmd Command) string

// DefaultStreamNamer is the default function used to determine the stream name
// for a given command when no custom StreamNamer is provided.
//
// By default, it returns the AggregateID of the command as the stream name.
//
// This variable can be overridden globally to change the default behavior
// for all command handlers, for example to support multi-tenancy, prefixes,
// or other custom naming conventions.
//
// Example usage:
//
//	// Default behavior uses AggregateID
//	stream := DefaultStreamNamer(ctx, myCommand)
//
//	// Override globally
//	DefaultStreamNamer = func(ctx context.Context, cmd Command) string {
//	    tenant := ctx.Value("tenant").(string)
//	    return fmt.Sprintf("%s-orders-%s", tenant, cmd.AggregateID())
//	}
var DefaultStreamNamer StreamNamer = func(ctx context.Context, cmd Command) string {
	return cmd.AggregateID()
}

// CommandHandler defines a function type for handling commands of a specific type.
//
// C represents the concrete command type implementing the Command interface.
//
// A CommandHandler is responsible for implementing the business logic associated
// with a command. This typically includes validation, orchestration, and producing
// side effects, such as persisting events to an EventStore or triggering other operations.
//
// Handlers of this type are generally registered with a CommandBus, which ensures that
// commands are dispatched to the correct handler based on their type.
//
// Parameters:
//   - ctx: The context for controlling cancellation, deadlines, and carrying request-scoped values.
//   - command: The command of type C, representing the intent to perform a domain action.
//
// Returns:
//   - AppendResult: Represents the result of handling the command, including success status,
//     the next expected version of the aggregate, and any events that were persisted.
//   - error: Non-nil if the command handling failed, e.g., due to validation errors, business rule violations,
//     or persistence failures.
//
// Notes:
//   - Implementations should treat the command as immutable.
//   - Any domain state changes should be expressed via events (AppendResult.Events) rather than directly mutating state.
//   - Handlers should not panic; all errors should be returned via the error return value.
//
// Example Usage:
//
//	func HandleReserveSeat(ctx context.Context, cmd ReserveSeat) (AppendResult, error) {
//	    if seatAlreadyReserved(cmd.SeatNumber) {
//	        return AppendResult{Successful: false}, fmt.Errorf("seat already reserved")
//	    }
//	    events := []Event{SeatReserved{SeatNumber: cmd.SeatNumber, UserID: cmd.UserID}}
//	    return AppendResult{Successful: true, Events: events}, nil
//	}
type CommandHandler[C Command] func(ctx context.Context, command C) (AppendResult, error)

// Evolver evolves the given state into a new state with the event applied.
//
// T represents the aggregate state type.
//
// Parameters:
//   - envelope: A  Envelope object representing an historical event
//     of an aggregate. The Envelope is consumed by the Evolver.
//
// Returns:go
//   - The reconstructed aggregate state of type T.
//
// Notes:
//   - The Evolver is responsible for applying the event  to the
//     current state, producing the latest state.
type Evolver[T any] func(currentState T, envelope *Envelope) T

// Decider determines which events should occur based on the current state and a command.
//
// T represents the aggregate state type.
// C represents the command type.
//
// Parameters:
//   - state: The current aggregate state as returned by the Evolver.
//   - cmd: The command to handle, containing the intent to change state.
//
// Returns:
//   - A slice of Event representing the events that should be applied to the aggregate.
//   - An error, which should be non-nil if the command violates business rules
//     or cannot be applied to the current state.
//
// Notes:
//   - The Decider should not mutate the input state directly; it should produce
//     events that, when applied via the Evolver, will update the state accordingly.
//   - Returning an empty slice indicates that the command produces no events
//     (e.g., it was idempotent or had no effect).
type Decider[T any, C Command] func(state T, cmd C) ([]Event, error)

// CommandHandlerOption defines a function type that modifies handlerOptions.
// These options are applied when constructing a NewCommandHandler to customize behavior.
type CommandHandlerOption func(configuration *handlerOptions)

// NewCommandHandler returns a generic command handler for any aggregate type.
//
// It provides a reusable pattern for handling commands in an event-sourced system
// by performing the following steps:
//  1. Load the event history for the aggregate (using LoadStreamFrom).
//  2. Evolve the current state based on the event history.
//  3. Decide which new events should occur based on the command and current state.
//  4. Wrap the decided events in envelopes, assigning version numbers and metadata.
//  5. Persist the envelopes to the EventStore, respecting the configured revision
//     and concurrency rules.
//
// Parameters:
//   - store: The EventStore used to load and persist events.
//   - evolve: A function of type Evolver[T] that reconstructs aggregate state from a sequence of events.
//   - decide: A function of type Decider[T, C] that produces events based on the current state and command.
//   - opts: Optional CommandHandlerOption values for customizing behavior, such as:
//   - StreamState: The expected stream revision (default is Any).
//   - RetryAttempts: Number of retries on version conflicts (default 0).
//
// Returns:
//   - A function that takes a context and a command of type C, and returns:
//   - AppendResult: Contains information about the persistence result, including success
//     and the next expected version.
//   - error: Non-nil if the command failed, either due to a business rule violation,
//     persistence error, or concurrency conflict.
//
// Behavior Details:
//   - The command’s AggregateID() is used to identify the target stream in the EventStore.
//   - The SeqWithSideEffect wrapper tracks the last version while evolving state.
//   - If the configured StreamState is Revision, it is updated to the latest version
//     before saving to ensure optimistic concurrency control.
//   - If the decide function returns no events, the handler returns a successful result without persisting.
//   - Each event is wrapped in an Envelope with a new UUID, metadata map, version, and timestamp.
//   - Errors during loading, evolving, deciding, or saving are propagated with context using errors.Wrap.
//
// Example Usage:
//
//	handler := NewCommandHandler(store, evolveFunc, decideFunc, WithStreamState(Any{}))
//	result, err := handler(ctx, myCommand)
func NewCommandHandler[T any, C Command](
	store EventStore,
	initialState T,
	evolve Evolver[T],
	decide Decider[T, C],
	opts ...CommandHandlerOption,
) CommandHandler[C] {
	// Apply handler options
	options := &handlerOptions{
		Revision:      Any{}, // default
		RetryStrategy: &backoff.StopBackOff{},
		MetadataFuncs: []func(ctx context.Context) map[string]any{},
		StreamNamer:   DefaultStreamNamer,
	}
	for _, o := range opts {
		o(options)
	}

	return func(ctx context.Context, command C) (AppendResult, error) {
		// the stream ID for the given command
		var streamID = options.StreamNamer(ctx, command)
		// the state we will decide against
		var state = initialState
		// the stream state we will save against
		var revision = options.Revision
		// the revision we
		var lastVersion uint64
		// Retry loop for handling concurrency conflicts
		result, err := backoff.RetryWithData(func() (AppendResult, error) {

			iter, err := store.LoadStreamFrom(ctx, streamID, revision)

			if err != nil {
				return AppendResult{Successful: false, StreamID: streamID, NextExpectedVersion: lastVersion},
					backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): load failed: %w", command, command.AggregateID(), streamID, err))
			}

			// --- Evolve state ---
			for iter.Next(ctx) {
				event := iter.Value()
				revision = Revision(event.Version)
				lastVersion = event.Version
				state = evolve(state, event)
			}

			if err := iter.Err(); err != nil {
				return AppendResult{Successful: false, StreamID: streamID, NextExpectedVersion: lastVersion},
					fmt.Errorf("handle command %T for aggregate %q (streamID %q): iter failed: %w", command, command.AggregateID(), streamID, err)
			}

			// --- Decide events ---
			events, err := decide(state, command)

			if err != nil {
				businessErr := errors.Join(err, ErrBusinessRuleViolation)
				return AppendResult{Successful: false, StreamID: streamID},
					backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): business rule violation: %w", command, command.AggregateID(), streamID, businessErr)) // business rule violation
			}

			// If no events, return success without saving
			if len(events) == 0 {
				// Nothing to persist
				return AppendResult{Successful: true, StreamID: streamID, NextExpectedVersion: lastVersion}, nil
			}

			// --- Wrap events in envelopes ---
			envelopes := make([]Envelope, len(events))
			baseMetadata := make(map[string]any)
			for _, fn := range options.MetadataFuncs {
				for k, v := range fn(ctx) {
					baseMetadata[k] = v
				}
			}

			nextVersion := lastVersion + 1

			for i, event := range events {
				envelopes[i] = Envelope{
					EventID:    uuid.New(),
					StreamID:   streamID,
					Event:      event,
					Metadata:   baseMetadata,
					Version:    nextVersion + uint64(i),
					OccurredAt: time.Now(),
				}
			}

			// --- Persist events ---
			result, err := store.Save(ctx, envelopes, options.Revision)

			if err != nil {
				var conflict *StreamRevisionConflictError
				if errors.As(err, &conflict) {
					// Retry on concurrency conflicts
					return AppendResult{Successful: false, NextExpectedVersion: lastVersion + 1, StreamID: streamID}, conflict
				}
				return result, backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): failed to save event: %w", command, command.AggregateID(), streamID, err))
			}
			return result, nil
		}, options.RetryStrategy)

		return result, err

	}
}

// handlerOptions defines configuration for a CommandHandler.
//
// It is used internally by NewCommandHandler to control behavior such as
// concurrency checks, retry strategy, and event metadata enrichment.
//
// Fields:
//   - StreamState: Determines the concurrency check applied when saving events.
//     Defaults to Any{}.
//   - RetryStrategy: Strategy for retrying operations in case of transient
//     failures or version conflicts. Defaults to no retries.
//   - MetadataFuncs: Slice of functions that generate metadata for each event.
//     Each function receives the context and returns a map of key-value pairs.
//   - StreamNamer: Function used to determine the stream name for a command.
//     If nil, DefaultStreamNamer is used, which returns the AggregateID by default.
//     This can be overridden per handler or globally for custom naming conventions
//     (e.g., multi-tenancy, dynamic prefixes, or other domain-specific logic).
type handlerOptions struct {
	// Revision is the condition applied when saving events to the stream.
	// It determines the concurrency check behavior (default is Any).
	Revision StreamState

	// RetryStrategy defines how the handler should retry operations in case of transient failures
	// or version conflicts. If nil, no retries are performed.
	RetryStrategy backoff.BackOff

	// MetadataFuncs is a list of functions used to enrich events with metadata before saving.
	// Each function receives the context and returns a map of key-value pairs.
	MetadataFuncs []func(ctx context.Context) map[string]any

	// StreamNamer produces the name of the event stream for a command.
	// If nil, DefaultStreamNamer is used.
	StreamNamer StreamNamer
}

// WithStreamState sets the expected stream revision for a NewCommandHandler.
//
// The StreamState controls the concurrency check when persisting events. For example:
//   - Any{}: no version check (default)
//   - NoStream{}: ensures the stream does not exist
//   - StreamExists{}: ensures the stream exists
//   - Revision{N}: expects the stream to be at version N
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithRevision(NoStream))
func WithStreamState(rev StreamState) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.Revision = rev }
}

// WithRetryStrategy sets the retry strategy for a NewCommandHandler.
//
// The BackOff strategy controls how many times and with what delay the handler
// retries saving events in case of concurrency conflicts or transient errors.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithRetryStrategy(myBackoff))
func WithRetryStrategy(strategy backoff.BackOff) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.RetryStrategy = strategy }
}

// WithMetadataExtractor adds a metadata function to a NewCommandHandler.
//
// Each metadata function is called for every command handling execution and can
// inject additional key-value pairs into the event envelopes. Multiple metadata
// extractors can be combined; they are applied in order of registration.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithMetadataExtractor(myMetadataFunc))
func WithMetadataExtractor(fn func(ctx context.Context) map[string]any) CommandHandlerOption {
	return func(h *handlerOptions) {
		h.MetadataFuncs = append(h.MetadataFuncs, fn)
	}
}

// WithStreamNamer adds stream namer to a NewCommandHandler.
//
// Each metadata function is called for every command handling execution and can
// inject additional key-value pairs into the event envelopes. Multiple metadata
// extractors can be combined; they are applied in order of registration.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithMetadataExtractor(myMetadataFunc))
func WithStreamNamer(namer StreamNamer) CommandHandlerOption {
	return func(h *handlerOptions) {
		h.StreamNamer = namer
	}
}
