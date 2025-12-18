package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
)

type Dispatcher interface {
	Dispatch(ctx context.Context, cmd Command) (AppendResult, error)
}

// queuedCommand represents a command enqueued in the command bus for processing.
// Each queuedCommand includes the context for cancellation, the command itself,
// and a response channel to return the processing result.
type queuedCommand struct {
	Ctx        context.Context
	Command    Command
	ResponseCh chan<- commandResult
}

// commandResult represents the result of processing a command.
// It contains the AppendResult (success/failure metadata) and any error
// encountered during command handling.
type commandResult struct {
	Result AppendResult
	Err    error
}

// CommandBus is an internal, in-memory, type-safe command dispatcher.
// It maintains a mapping of command type names to their handlers, a queues for
// incoming commands, and synchronization mechanisms for safe concurrent access.
//
// The CommandBus supports:
//   - Enqueuing commands for asynchronous processing
//   - Typed command registration using generics
//   - Safe shutdown that waits for in-flight commands to complete
//   - Panic recovery in handlers to prevent the bus from crashing
type CommandBus struct {
	handlers   map[string]func(ctx context.Context, command Command) (AppendResult, error)
	queues     []chan queuedCommand
	stopCh     chan struct{}
	wg         sync.WaitGroup
	mu         sync.RWMutex
	shardCount int
}

// NewCommandBus creates a new instance of CommandBus with a buffered queues.
//
// Parameters:
//   - bufferSize: the size of the internal queues for enqueued commands.
//
// Returns:
//   - pointer to a newly initialized CommandBus. The internal processing
//     goroutine is started automatically.
//
// Example:
//
//	bus := NewCommandBus(100)
func NewCommandBus(bufferSize int, shardCount int) *CommandBus {

	if shardCount <= 0 {
		shardCount = 1
	}

	bus := &CommandBus{
		queues:     make([]chan queuedCommand, shardCount),
		handlers:   make(map[string]func(ctx context.Context, command Command) (AppendResult, error)),
		stopCh:     make(chan struct{}),
		shardCount: shardCount,
	}

	for i := 0; i < shardCount; i++ {
		bus.queues[i] = make(chan queuedCommand, bufferSize)
		go bus.worker(bus.queues[i])
	}

	return bus
}

// Dispatch enqueues a command for processing by the registered handler and
// waits for the result. It is safe to call concurrently.
//
// Parameters:
//   - ctx: the context for cancellation or timeout
//   - cmd: the command to dispatch
//
// Returns:
//   - AppendResult: indicates success/failure of command processing
//   - error: non-nil if the dispatch failed due to context cancellation or processing error
//
// Notes:
//   - Returns an error immediately if the bus has been stopped.
//   - Waits for the handler to complete and sends the result back via a response channel.
func (b *CommandBus) Dispatch(ctx context.Context, cmd Command) (AppendResult, error) {
	select {
	case <-b.stopCh:
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %s for aggregate %q: %w", TypeName(cmd), cmd.AggregateID(), ErrCommandBusClosed)
	default:
	}

	responseCh := make(chan commandResult, 1)
	b.wg.Add(1)
	defer b.wg.Done()

	shard := b.getShard(cmd.AggregateID())

	// Enqueue the command with the response channel
	select {
	case b.queues[shard] <- queuedCommand{Ctx: ctx, Command: cmd, ResponseCh: responseCh}:
		// Wait for processing result
		select {
		case result := <-responseCh:
			if result.Err != nil {
				return result.Result, fmt.Errorf("dispatch command %s for aggregate %q: %w", TypeName(cmd), cmd.AggregateID(), result.Err)
			}
			return result.Result, nil
		case <-ctx.Done():
			return AppendResult{Successful: false}, fmt.Errorf("dispatch command %s for aggregate %q: %w", TypeName(cmd), cmd.AggregateID(), ctx.Err()) // Context timeout/cancellation
		}
	case <-ctx.Done():
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %s for aggregate %q: %w", TypeName(cmd), cmd.AggregateID(), ctx.Err()) // Context timeout before enqueueing
	}
}

// worker processes commands from a single shard queues.
func (b *CommandBus) worker(queue chan queuedCommand) {
	for cmd := range queue {
		cmdName := fmt.Sprintf("%T", cmd.Command)

		b.mu.RLock()
		h, exists := b.handlers[cmdName]
		b.mu.RUnlock()

		if !exists {
			cmd.ResponseCh <- commandResult{
				Result: AppendResult{Successful: false},
				Err: fmt.Errorf(
					"dispatch command %s for aggregate %q: %w",
					cmdName, TypeName(cmd), ErrHandlerNotRegistered,
				),
			}
			continue
		}

		func() {
			defer func() {
				if r := recover(); r != nil {

					var panicErr error

					if e, ok := r.(error); ok {
						panicErr = e
					} else {
						panicErr = fmt.Errorf("panic: %v", r)
					}

					err := errors.Join(panicErr, ErrHandlerPanicked)

					cmd.ResponseCh <- commandResult{
						Result: AppendResult{Successful: false},
						//TODO improve the error. should it just be "UnrecoverableErr when handling Command ?
						Err: fmt.Errorf(
							"handling command %s for aggregate %q: %w",
							cmdName, cmd.Command.AggregateID(), err,
						),
					}
				}
			}()

			res, err := h(cmd.Ctx, cmd.Command)
			cmd.ResponseCh <- commandResult{Result: res, Err: err}
		}()
	}
}

func (b *CommandBus) getShard(aggregateID string) int {
	hash := fnv.New32a()
	hash.Write([]byte(aggregateID))
	return int(hash.Sum32()) % b.shardCount
}

// Register adds a new typed command handler to the bus.
//
// Parameters:
//   - b: pointer to the CommandBus
//   - handler: a generic CommandHandler[Command] function for a specific command type C
//
// Notes:
//   - Derives the command type name automatically using fmt.Sprintf("%T") to avoid
//     manual registration strings.
//   - Panics if a handler is already registered for the same command type.
//
// Example:
//
//	err := Register(bus, fooHandler)
func Register[C Command](b *CommandBus, handler CommandHandler[C]) {

	var zero C

	cmdName := fmt.Sprintf("%T", zero)
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.handlers[cmdName]; exists {
		panic(fmt.Errorf("handler already registered for command type %s %w", cmdName, ErrDuplicateHandler))
	}

	b.handlers[cmdName] = func(ctx context.Context, cmd Command) (AppendResult, error) {
		c, ok := cmd.(C)
		if !ok {
			return AppendResult{Successful: false}, fmt.Errorf("expected command type %s but got %T", cmdName, cmd)
		}
		return handler(ctx, c)
	}
}

// Stop shuts down the CommandBus safely.
//
// Behavior:
//   - Stops accepting new commands.
//   - Closes the internal queues channel.
//   - Waits for all in-flight commands to finish before returning.
//
// Example:
//
//	bus.Stop()
func (b *CommandBus) Stop() {
	close(b.stopCh)
	for _, q := range b.queues {
		close(q)
	}
	b.wg.Wait()
}
