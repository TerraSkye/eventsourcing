package eventsourcing

import (
	"context"
	"fmt"
	"sync"
)

type GenericCommandHandler[C Command] func(ctx context.Context, command C) (AppendResult, error)

// CommandBus defines the contract for dispatching commands in the system.
type CommandBus interface {
	// RegisterHandler registers a handler for a specific command type.
	// Panics if a handler is already registered for that type.
	RegisterHandler[C Command](handler GenericCommandHandler[C])

	// Dispatch sends a command to its registered handler and returns the events and error.
	// Returns an error if no handler is registered or if type conversion fails.
	Dispatch(ctx context.Context, cmd Command) (AppendResult, error)
}

type CommandBus interface {
	Send(ctx context.Context, cmd Command) error
	AddHandler(handler func(ctx context.Context, command Command) error)
}

// CommandHandler is a generic handler for a specific command type

type CommandWithCtx struct {
	Ctx        context.Context
	Command    Command
	ResponseCh chan<- commandBusResult
}

type commandBusResult struct {
	AppendResult
	error
}

type commandBus struct {
	handlers map[string]func(ctx context.Context, command Command) error
	queue    chan CommandWithCtx
	mu       sync.RWMutex
}

func NewCommandBus(bufferSize int) CommandBus {
	bus := &commandBus{
		queue: make(chan CommandWithCtx, bufferSize),
	}

	go bus.start()
	return bus
}

func (b *commandBus) Send(ctx context.Context, cmd Command) error {
	responseCh := make(chan commandBusResult, 1)

	// Enqueue the command with the response channel
	select {
	case b.queue <- CommandWithCtx{Ctx: ctx, Command: cmd, ResponseCh: responseCh}:
		// Wait for processing result
		select {
		case err := <-responseCh:
			return err // Return processing error (or nil if success)
		case <-ctx.Done():
			return ctx.Err() // Context timeout/cancellation
		}

	case <-ctx.Done():
		return ctx.Err() // Context timeout before enqueueing
	}
}

// RegisterHandler registers a command handler for a specific command type.
// Panics if a handler is already registered for the type.
func (b *commandBus) RegisterHandler[C Command](handler GenericCommandHandler[C]) {
	b.mu.Lock()
	defer b.mu.Unlock()

	cmdName := TypeName((*C)(nil))
	if _, exists := b.handlers[cmdName]; exists {
		panic(fmt.Sprintf("handler already registered for command type %s", cmdName))
	}

	// Wrap the typed handler with a type assertion
	b.handlers[cmdName] = func(ctx context.Context, command Command) ([]Event, error) {
		cmd, ok := command.(C)
		if !ok {
			return nil, fmt.Errorf("invalid command type: expected %T, got %T", *new(C), command)
		}
		return handler(ctx, cmd)
	}
}

func (b *commandBus) start() {
	go func() {
		for cmdWithCtx := range b.queue {

			routeTo := TypeName(cmdWithCtx.Command)

			h, exists := b.handlers[routeTo]

			if !exists {
				cmdWithCtx.ResponseCh <- commandBusResult{
					AppendResult: AppendResult{Successful: false},
					error:        fmt.Errorf("command %s not found", routeTo),
				}
				continue
			}

			result, err := h(cmdWithCtx.Ctx, cmdWithCtx.Command)

			cmdWithCtx.ResponseCh <- commandBusResult{
				AppendResult: result,
				error:        err,
			}

			for _, handler := range b.handlers {
				go func(handlerFunc func(ctx context.Context, command Command) error) {
					err := handlerFunc(cmdWithCtx.Ctx, cmdWithCtx.Command)
					if err != nil {
						cmdWithCtx.ResponseCh <- err
					} else {
						cmdWithCtx.ResponseCh <- nil
					}
				}(handler)
			}
		}
	}()
}
