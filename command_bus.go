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
	handlers    map[string]CommandHandler[Command]
	queues      []chan queuedCommand
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
	mu          sync.Mutex
	shardCount  int
	middlewares []CommandHandlerMiddleware
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
		queues:      make([]chan queuedCommand, shardCount),
		handlers:    make(map[string]CommandHandler[Command]),
		middlewares: make([]CommandHandlerMiddleware, 0),
		stopCh:      make(chan struct{}),
		shardCount:  shardCount,
	}

	for i := 0; i < shardCount; i++ {
		bus.queues[i] = make(chan queuedCommand, bufferSize)
		go bus.worker(bus.queues[i])
	}

	return bus
}

// Dispatch enqueues cmd for the handler registered for its type and blocks
// until that handler returns. Commands for the same aggregate are routed to the
// same shard, and each shard is drained by a single worker, so they are never
// handled concurrently. Dispatch itself is safe to call concurrently.
//
// The returned [AppendResult] carries the outcome of the append. The error is
// non-nil if ctx is cancelled before the command is enqueued or before the
// result arrives, if no handler is registered for the command's type, if the
// handler returns an error or panics, or if the bus has been stopped. Once the
// bus is stopped the error wraps [ErrCommandBusClosed], including for a call
// already parked waiting to enqueue.
func (b *CommandBus) Dispatch(ctx context.Context, cmd Command) (AppendResult, error) {
	// Registering as in-flight must be atomic with the stopCh check: sync.WaitGroup
	// forbids an Add that races a Wait, and Stop calls Wait as soon as it has closed
	// stopCh. Holding b.mu across both makes every Add happen-before that close, so
	// Stop is a real barrier and go test -race stays clean.
	b.mu.Lock()
	select {
	case <-b.stopCh:
		b.mu.Unlock()
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), ErrCommandBusClosed)
	default:
	}
	b.wg.Add(1)
	b.mu.Unlock()
	defer b.wg.Done()

	responseCh := make(chan commandResult, 1)

	shard := b.selectShard(cmd.AggregateID())

	// Enqueue the command with the response channel
	select {
	case b.queues[shard] <- queuedCommand{Ctx: ctx, Command: cmd, ResponseCh: responseCh}:
		// Wait for processing result
	case <-b.stopCh:
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), ErrCommandBusClosed)
	case <-ctx.Done():
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), ctx.Err()) // Context timeout before enqueueing
	}

	select {
	case result := <-responseCh:
		if result.Err != nil {
			return result.Result, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), result.Err)
		}
		return result.Result, nil
	case <-ctx.Done():
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), ctx.Err()) // Context timeout/cancellation
	}
}

// worker processes commands from a single shard queues.
func (b *CommandBus) worker(queue chan queuedCommand) {

	for {
		var cmd queuedCommand

		select {
		case cmd = <-queue:
		case <-b.stopCh:
			// drain whatever is still queued, then exit
			select {
			case cmd = <-queue:
			default:
				return
			}
		}

		cmdName := fmt.Sprintf("%T", cmd.Command)

		h, exists := b.handlers[cmdName]

		if !exists {
			cmd.ResponseCh <- commandResult{
				Result: AppendResult{Successful: false},
				Err: fmt.Errorf(
					"dispatch command %s for aggregate %q: %w",
					cmdName, cmd.Command.AggregateID(), ErrHandlerNotRegistered,
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
							"handling command %T for aggregate %q: %w",
							cmd.Command, cmd.Command.AggregateID(), err,
						),
					}
				}
			}()

			res, err := h(cmd.Ctx, cmd.Command)
			cmd.ResponseCh <- commandResult{Result: res, Err: err}
		}()
	}
}

func (b *CommandBus) selectShard(aggregateID string) int {
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
//   - The middleware chain is applied at registration time; call Use before Register.
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

	h := CommandHandler[Command](func(ctx context.Context, cmd Command) (AppendResult, error) {
		c, ok := cmd.(C)
		if !ok {
			return AppendResult{Successful: false}, fmt.Errorf("expected command type %s but got %T", cmdName, cmd)
		}
		return handler(ctx, c)
	})
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		h = b.middlewares[i](h)
	}
	b.handlers[cmdName] = h
}

// Stop shuts down the bus. It stops accepting new commands, lets the workers
// finish whatever is already queued, and waits for every in-flight [CommandBus.Dispatch]
// to return before it does.
//
// A Dispatch racing Stop either completes normally or fails with an error
// wrapping [ErrCommandBusClosed]; it never panics. Stop is idempotent and safe
// to call concurrently, but the bus cannot be restarted afterwards.
func (b *CommandBus) Stop() {
	// Close under b.mu so it cannot land between Dispatch's stopCh check and its
	// wg.Add. Once this returns, every subsequent Dispatch observes stopCh closed
	// and bails out without touching wg, so Wait sees a final count.
	b.mu.Lock()
	b.stopOnce.Do(func() { close(b.stopCh) })
	b.mu.Unlock()

	b.wg.Wait()
}
