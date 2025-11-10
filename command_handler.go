package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

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
// These options are applied when constructing a CommandHandler to customize behavior.
type CommandHandlerOption func(configuration *handlerOptions)

// CommandHandler returns a generic command handler for any aggregate type.
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
//   - Revision: The expected stream revision (default is Any).
//   - RetryAttempts: Number of retries on version conflicts (default 0).
//   - RetryDelay: Delay between retries (default 100ms).
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
//   - If the configured Revision is ExplicitRevision, it is updated to the latest version
//     before saving to ensure optimistic concurrency control.
//   - If the decide function returns no events, the handler returns a successful result without persisting.
//   - Each event is wrapped in a Envelope with a new UUID, metadata map, version, and timestamp.
//   - Errors during loading, evolving, deciding, or saving are propagated with context using errors.Wrap.
//
// Example Usage:
//
//	handler := CommandHandler(store, evolveFunc, decideFunc, WithRevision(Any{}))
//	result, err := handler(ctx, myCommand)
func CommandHandler[T any, C Command](
	store EventStore,
	initialState T,
	evolve Evolver[T],
	decide Decider[T, C],
	opts ...CommandHandlerOption,
) func(ctx context.Context, command C) (AppendResult, error) {
	return func(ctx context.Context, command C) (AppendResult, error) {
		cfg := &handlerOptions{
			Revision:      Any{}, // default
			RetryStrategy: &backoff.StopBackOff{},
			MetadataFuncs: make([]func(ctx context.Context) map[string]any, 0),
		}
		for _, o := range opts {
			o(cfg)
		}

		state := initialState

		var revision uint64

		result, err := backoff.RetryWithData(func() (AppendResult, error) {

			// Load event history for the aggregate
			history, err := store.LoadStreamFrom(ctx, command.AggregateID(), revision)
			if err != nil {
				// when failing to load of the event stream. this is the error.
				return AppendResult{Successful: false}, backoff.Permanent(fmt.Errorf("failed to load event stream: %w", err))
			}

			for envelope := range history {
				revision = envelope.Version
				state = evolve(state, envelope)
			}

			if _, shouldBeExact := cfg.Revision.(ExplicitRevision); shouldBeExact {
				cfg.Revision = ExplicitRevision(revision)
			}

			// Decide what events should occur based on the command and state
			events, err := decide(state, command)

			if err != nil {
				return AppendResult{Successful: false}, backoff.Permanent(err) // business rule violation
			}

			if len(events) == 0 {
				// Nothing to persist
				return AppendResult{Successful: true, NextExpectedVersion: revision}, nil
			}

			expectedVersion := revision
			// Wrap events in envelopes
			envelopes := make([]Envelope, len(events))

			baseMetadata := make(map[string]any)
			for _, fn := range cfg.MetadataFuncs {
				for k, v := range fn(ctx) {
					baseMetadata[k] = v
				}
			}

			for i, event := range events {
				expectedVersion++
				envelopes[i] = Envelope{
					UUID:       uuid.New(),
					Event:      event,
					Metadata:   baseMetadata,
					Version:    expectedVersion,
					OccurredAt: time.Now(),
				}
			}

			// Persist the events
			if result, err := store.Save(ctx, envelopes, cfg.Revision); err != nil {
				var conflict *StreamRevisionConflictError
				if errors.As(err, &conflict) {
					// we will retry concurrency errors
					return AppendResult{Successful: false, NextExpectedVersion: revision}, conflict
				}

				// an event store error occurred. could not save the events
				return result, backoff.Permanent(fmt.Errorf("failed to save events: %w", err))
			} else {
				return result, nil
			}
		}, cfg.RetryStrategy)

		return result, err

	}
}

type handlerOptions struct {
	// Revision is the condition we apply on the stream when saving (defaults to current state )
	Revision Revision
	// RetryStrategy is the strategy to retry on version-conflicts version-conflict. (default none)
	RetryStrategy backoff.BackOff
	// ShouldRetry is an optional hook to decide whether a given error should be retried.
	// If nil, only ErrVersionConflict is considered retryable.
	ShouldRetry func(error) bool
	// MetadataFuncs is is an
	MetadataFuncs []func(ctx context.Context) map[string]any
}

// WithRevision sets the expected stream revision for a CommandHandler.
//
// Parameters:
//   - rev: The Revision value that determines the concurrency check
//     when saving events (e.g., Any, NoStream, StreamExists, ExplicitRevision).
//
// Usage:
//
//	handler := CommandHandler(store, evolve, decide, WithRevision(NoStream))
func WithRevision(rev Revision) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.Revision = rev }
}

// WithRevision sets the expected stream revision for a CommandHandler.
//
// Parameters:
//   - rev: The Revision value that determines the concurrency check
//     when saving events (e.g., Any, NoStream, StreamExists, ExplicitRevision).
//
// Usage:
//
//	handler := CommandHandler(store, evolve, decide, WithRevision(NoStream))
func WithRetryStrategy(backoff backoff.BackOff) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.RetryStrategy = backoff }
}

func WithMetadataExtractor(fn func(ctx context.Context) map[string]any) CommandHandlerOption {
	return func(h *handlerOptions) {
		h.MetadataFuncs = append(h.MetadataFuncs, fn)
	}
}
