package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
)

// Dispatcher has the same signature as [CommandBus.Dispatch], for code that
// wants to depend on the dispatch behavior without depending on *CommandBus
// itself.
type Dispatcher interface {
	Dispatch(ctx context.Context, cmd Command) (AppendResult, error)
}

// queuedCommand is a command enqueued on a [CommandBus] shard, carrying the
// context to honor for cancellation and the channel to deliver the result on.
type queuedCommand struct {
	Ctx        context.Context
	Command    Command
	ResponseCh chan<- commandResult
}

// commandResult is the outcome of processing a queuedCommand: the resulting
// [AppendResult] and any error from the handler.
type commandResult struct {
	Result AppendResult
	Err    error
}

// CommandBus is an in-memory, type-safe command dispatcher. It keeps a
// registry of command type names to handlers, sharded queues of incoming
// commands, and the synchronization needed for concurrent [Dispatch] and
// [Register] calls. Commands for a given aggregate are always routed to the
// same shard and processed one at a time, [Stop] waits for in-flight
// dispatches to finish, and a handler panic is recovered rather than
// crashing the bus.
type CommandBus struct {
	handlers    map[string]CommandHandler[Command]
	queues      []chan queuedCommand
	stopCh      chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
	mu          sync.RWMutex
	shardCount  int
	middlewares []CommandHandlerMiddleware
}

// NewCommandBus returns a [CommandBus] with shardCount shards, each with a
// worker goroutine already running and a queue buffering up to bufferSize
// commands. shardCount is clamped to at least 1.
//
// Example:
//
//	bus := NewCommandBus(100, 4)
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
	// stopCh. Stop takes the write lock to close, so it waits out every reader here
	// and every Add happens-before the close. Concurrent dispatches hold the read
	// lock together and do not serialize.
	b.mu.RLock()
	select {
	case <-b.stopCh:
		b.mu.RUnlock()
		return AppendResult{Successful: false}, fmt.Errorf("dispatch command %T for aggregate %q: %w", cmd, cmd.AggregateID(), ErrCommandBusClosed)
	default:
	}
	b.wg.Add(1)
	b.mu.RUnlock()
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

// worker processes commands from a single shard's queue.
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

		h, exists := b.handlerFor(cmdName)

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

// handlerFor returns the handler registered for cmdName, and reports whether one
// exists. The lock is held only for the map lookup, never for the handler call,
// so a slow handler cannot block Register.
func (b *CommandBus) handlerFor(cmdName string) (CommandHandler[Command], bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	h, ok := b.handlers[cmdName]
	return h, ok
}

func (b *CommandBus) selectShard(aggregateID string) int {
	hash := fnv.New32a()
	hash.Write([]byte(aggregateID))
	return int(hash.Sum32()) % b.shardCount
}

// Register installs handler as the handler for command type C on b. The
// registration key is derived from C with fmt.Sprintf("%T"), so there are no
// manual type strings to keep in sync. Register panics with
// [ErrDuplicateHandler] if a handler for C is already registered.
//
// The middleware chain is applied here, at registration time, from the
// middlewares added via [CommandBus.Use] up to this point — so call Use first.
// Register is safe to call on a bus that is already dispatching, though wiring
// every handler during startup is the expected pattern.
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
