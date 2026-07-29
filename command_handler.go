package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/google/uuid"
)

// StreamNamer maps a [Command] to the name of the event stream it belongs to.
type StreamNamer func(ctx context.Context, cmd Command) string

// DefaultStreamNamer is the [StreamNamer] used by command handlers when no
// custom namer is configured. By default it returns the command's
// [Command.AggregateID] as the stream name.
//
// It can be overridden globally, for example to add tenant prefixes for
// multi-tenancy. Override it during program initialization, before any
// handler runs, to avoid data races on the global.
//
//	// During startup:
//	DefaultStreamNamer = func(ctx context.Context, cmd Command) string {
//		tenant, _ := ctx.Value(tenantKey).(string)
//		return fmt.Sprintf("%s-orders-%s", tenant, cmd.AggregateID())
//	}
var DefaultStreamNamer StreamNamer = func(ctx context.Context, cmd Command) string {
	return cmd.AggregateID()
}

// CommandHandler handles commands of the concrete type C, which must
// implement [Command]. A handler carries out the business logic for a
// command — validating it, deciding what should happen, and persisting any
// resulting events — and is typically registered with a [CommandBus] via
// [Register], which dispatches each command to the handler registered for
// its type.
//
// It returns an [AppendResult] describing the outcome and a non-nil error
// if handling failed, for example due to a validation error, a business-rule
// violation, or a persistence failure. Implementations should treat the
// command as immutable, express state changes as events persisted to an
// [EventStore] rather than by mutating in-memory state, and return errors
// rather than panicking.
//
// Example Usage:
//
//	func HandleReserveSeat(ctx context.Context, cmd ReserveSeat) (AppendResult, error) {
//	    if seatAlreadyReserved(cmd.SeatNumber) {
//	        return AppendResult{Successful: false}, fmt.Errorf("seat already reserved")
//	    }
//	    // persist a SeatReserved event via an EventStore, then:
//	    return AppendResult{Successful: true, StreamID: cmd.AggregateID()}, nil
//	}
type CommandHandler[C Command] func(ctx context.Context, command C) (AppendResult, error)

// InitialState returns the zero-event aggregate state of type T, before any
// events have been evolved into it — for example, an empty struct or one
// with its fields set to their defaults.
type InitialState[T any] func() T

// Evolver applies a single historical event to currentState and returns the
// resulting aggregate state of type T. It must not mutate currentState;
// [NewCommandHandler] calls it once per event, in stream order, folding the
// result of each call into the next.
type Evolver[T any] func(currentState T, envelope *Envelope) T

// Decider inspects state, as produced by an [Evolver], against cmd and
// returns the events that should occur as a result, or a non-nil error if
// cmd violates a business rule or cannot be applied to state. It must not
// mutate state; state changes are expressed only through the returned
// events. An empty, nil slice means cmd produces no events, for example
// because it was idempotent or had no effect.
type Decider[T any, C Command] func(state T, cmd C) ([]Event, error)

// CommandHandlerOption configures a [CommandHandler] built by
// [NewCommandHandler].
type CommandHandlerOption func(configuration *handlerOptions)

// NewCommandHandler returns a generic command handler for any aggregate type.
//
// The returned handler, on each call: loads command's stream from store
// (named by [DefaultStreamNamer], or a [StreamNamer] set via
// [WithStreamNamer]) using [EventStore.LoadStreamFrom]; folds every loaded
// event into initialState with evolve; passes the resulting state and the
// command to decide to get the events to persist; wraps them in
// [Envelope]s, stamping each with a new UUID, the next sequential version,
// the current time, and any metadata from functions added via
// [WithMetadataExtractor]; and saves them with [EventStore.Save].
//
// The [StreamState] configured via [WithStreamState] (default [Any])
// determines how a save conflict is handled. [Any] and [StreamExists] don't
// pin the stream to an exact version — [Any] asserts nothing, and
// [StreamExists] only asserts non-emptiness, once loading has confirmed the
// stream exists — so the handler saves against the revision it just loaded
// and, on conflict, retries the whole load-evolve-decide-save cycle
// according to the [backoff.BackOff] set via [WithRetryStrategy] (default:
// no retries). [NoStream] and a specific [Revision] pin the stream to an
// exact point (version 0, or N) that the caller explicitly asserted; a
// conflict there means that expectation was violated, so it is returned
// immediately rather than retried. A [StreamExists] load failure (the
// stream doesn't exist yet) is likewise always returned immediately, never
// retried — it's a fail-fast precondition, not a save conflict to converge
// on. Any other Save or load error is also returned directly, without
// retrying.
//
// If decide returns no events, the handler returns a successful
// [AppendResult] without calling Save. If decide returns a non-nil error,
// the handler returns it wrapped in an [ErrBusinessRuleViolation].
//
// Example Usage:
//
//	handler := NewCommandHandler(store, initialStateFunc, evolveFunc, decideFunc, WithStreamState(Any{}))
//	result, err := handler(ctx, myCommand)
func NewCommandHandler[T any, C Command](
	store EventStore,
	initialState InitialState[T],
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

	// Revision and NoStream pin the stream to an exact point (a specific
	// version, or version 0) that the caller explicitly asserted; retrying
	// past that point would silently violate the expectation, so a
	// conflict there is reported back immediately. That's also true of
	// StreamExists failing to load: the stream doesn't exist (yet), which
	// is a fail-fast condition, not a save conflict to converge on. But
	// once a StreamExists load succeeds, the stream's existence is settled
	// and only its version remains in question — from that point on it
	// behaves like Any (no version pinned), tracking the version loaded
	// and retrying on a save conflict.
	_, isAny := options.Revision.(Any)
	_, isStreamExists := options.Revision.(StreamExists)
	autoConverge := isAny || isStreamExists

	return func(ctx context.Context, command C) (AppendResult, error) {
		// the stream ID for the given command
		var streamID = options.StreamNamer(ctx, command)
		// the state we will decide against
		var state = initialState()
		// the stream state we will load from, and (when autoConverge) save against
		var revision = options.Revision
		// the last version loaded from the stream
		var lastVersion uint64
		// Retry loop for handling concurrency conflicts
		result, err := backoff.RetryWithData(func() (AppendResult, error) {

			iter, err := store.LoadStreamFrom(ctx, streamID, revision)

			if err != nil {
				// Even for StreamExists, a missing stream fails fast here:
				// the "auto-converge, don't pin a version" treatment only
				// applies once the stream is confirmed to exist — this load
				// error means it doesn't (yet), which is not something a
				// save-conflict retry cycle can resolve.
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
					backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): iter failed: %w", command, command.AggregateID(), streamID, err))
			}

			// --- Decide events ---
			events, err := decide(state, command)

			if err != nil {
				return AppendResult{Successful: false, StreamID: streamID},
					backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): business rule violation: %w", command, command.AggregateID(), streamID, NewBusinessRuleViolation(err)))
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
				maps.Copy(baseMetadata, fn(ctx))
			}

			nextVersion := lastVersion + 1

			for i, event := range events {
				envelopes[i] = Envelope{
					EventID:    uuid.New(),
					StreamID:   streamID,
					Event:      event,
					Metadata:   maps.Clone(baseMetadata),
					Version:    nextVersion + uint64(i),
					OccurredAt: time.Now(),
				}
			}

			// --- Persist events ---
			// With Any{} or StreamExists{}, save against the revision just
			// loaded above (Revision(lastVersion)), so a conflict can be
			// resolved by reloading and retrying. Revision(N) and NoStream{}
			// pin the stream to an exact point the caller asserted, so they
			// are saved against unchanged.
			saveRevision := options.Revision
			if autoConverge {
				saveRevision = revision
			}
			result, err := store.Save(ctx, envelopes, saveRevision)

			if err != nil {
				var conflict *StreamRevisionConflictError
				if errors.As(err, &conflict) {
					if autoConverge {
						// Retry: reload from `revision` and try again.
						return AppendResult{Successful: false, NextExpectedVersion: lastVersion + 1, StreamID: streamID}, conflict
					}
					// The caller asserted a specific stream expectation
					// (Revision or NoStream); a conflict means that
					// expectation was violated, so it is reported back
					// directly instead of being retried against a different
					// target.
					return AppendResult{Successful: false, NextExpectedVersion: lastVersion + 1, StreamID: streamID},
						backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): concurrency conflict: %w", command, command.AggregateID(), streamID, conflict))
				}
				return result, backoff.Permanent(fmt.Errorf("handle command %T for aggregate %q (streamID %q): failed to save event: %w", command, command.AggregateID(), streamID, err))
			}
			return result, nil
		}, options.RetryStrategy)

		return result, err

	}
}

// handlerOptions holds the configuration [NewCommandHandler] builds from
// its [CommandHandlerOption] arguments.
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

// WithStreamState sets the [StreamState] a [NewCommandHandler] expects the
// stream to be in when it saves events — [Any] (the default) to save
// against the revision the handler just loaded, or [StreamExists] to do the
// same after additionally requiring the stream to already exist; both retry
// on save conflict per [WithRetryStrategy], since neither pins an exact
// version. (A [StreamExists] load failure — the stream not existing yet —
// is still a fail-fast precondition and is always returned immediately.)
// [NoStream] (stream must not exist) or a specific [Revision] (stream must
// be at exactly that version) pin the stream to an exact point the caller
// explicitly asserted; a conflict there is always returned immediately,
// never retried, since retrying would silently move past that point.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithStreamState(NoStream{}))
func WithStreamState(rev StreamState) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.Revision = rev }
}

// WithRetryStrategy sets the [backoff.BackOff] a [NewCommandHandler] uses to
// retry its load-evolve-decide-save cycle after a save conflict, when the
// handler is configured (via [WithStreamState]) with [Any] (the default) or
// [StreamExists] — neither pins the stream to an exact version, so a save
// conflict can be resolved by retrying. It does not apply to a [StreamExists]
// load failure (the stream not existing yet), which is always returned
// immediately. Without this option, no retries are performed and the
// handler returns the conflict directly. It has no effect when
// [WithStreamState] is set to [NoStream] or a specific [Revision] — those
// pin an exact point, so a conflict there is always returned immediately.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithRetryStrategy(myBackoff))
func WithRetryStrategy(strategy backoff.BackOff) CommandHandlerOption {
	return func(cfg *handlerOptions) { cfg.RetryStrategy = strategy }
}

// WithMetadataExtractor adds fn to the metadata functions a
// [NewCommandHandler] calls for every command, merging their results into
// each resulting [Envelope]'s Metadata. Multiple extractors can be combined;
// they run in the order they were added, later ones overwriting keys set by
// earlier ones.
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithMetadataExtractor(myMetadataFunc))
func WithMetadataExtractor(fn func(ctx context.Context) map[string]any) CommandHandlerOption {
	return func(h *handlerOptions) {
		h.MetadataFuncs = append(h.MetadataFuncs, fn)
	}
}

// WithStreamNamer overrides the [StreamNamer] a [NewCommandHandler] uses to
// name the stream it loads and saves to, in place of [DefaultStreamNamer].
//
// Usage:
//
//	handler := NewCommandHandler(store, initialState, evolve, decide, WithStreamNamer(myStreamNamer))
func WithStreamNamer(namer StreamNamer) CommandHandlerOption {
	return func(h *handlerOptions) {
		h.StreamNamer = namer
	}
}
