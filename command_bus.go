package eventsourcing

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"os"
	"sync"
)

type CommandBus interface {
	Send(ctx context.Context, cmd Command) error
	AddHandler(handler func(ctx context.Context, command Command) error)
}

type CommandWithCtx struct {
	Ctx        context.Context
	Command    Command
	ResponseCh chan<- error
}

type commandBus struct {
	tracer   trace.Tracer
	handlers []func(ctx context.Context, command Command) error
	queue    chan CommandWithCtx
	sync.RWMutex
}

func NewCommandBus(bufferSize int) CommandBus {
	bus := &commandBus{
		queue:  make(chan CommandWithCtx, bufferSize),
		tracer: otel.Tracer("command-bus"),
	}

	go bus.start()
	return bus
}

func (b *commandBus) Send(ctx context.Context, cmd Command) error {
	responseCh := make(chan error, 1)

	// Start tracing
	ctx, span := b.tracer.Start(ctx, "cqrs.command.send",
		trace.WithAttributes(

			attribute.String("cqrs.aggregate_id", cmd.AggregateID().String()),
			attribute.String("cqrs.application", os.Getenv("application")),
			attribute.String("cqrs.causation_id", MustExtractCausationId(ctx)),
			attribute.String("cqrs.correlation_id", trace.SpanContextFromContext(ctx).TraceID().String()),
			attribute.String("cqrs.command", TypeName(cmd)),
			// Messaging attributes
			attribute.String("messaging.conversation_id", trace.SpanContextFromContext(ctx).TraceID().String()),
			attribute.String("messaging.destination_kind", "aggregate"),
			attribute.String("messaging.message_id", MustExtractCausationId(ctx)),
			attribute.String("messaging.operation", "publish"),
			attribute.String("messaging.system", "cqrs"),
		),
	)

	defer span.End()

	// Enqueue the command with the response channel
	select {
	case b.queue <- CommandWithCtx{Ctx: ctx, Command: cmd, ResponseCh: responseCh}:
		// Wait for processing result
		select {
		case err := <-responseCh:
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				span.SetStatus(codes.Ok, "")
			}
			return err // Return processing error (or nil if success)
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, ctx.Err().Error())
			return ctx.Err() // Context timeout/cancellation
		}

	case <-ctx.Done():
		span.RecordError(ctx.Err())
		span.SetStatus(codes.Error, ctx.Err().Error())
		return ctx.Err() // Context timeout before enqueueing
	}
}

func (b *commandBus) AddHandler(handler func(ctx context.Context, command Command) error) {
	b.Lock()
	defer b.Unlock()
	b.handlers = append(b.handlers, handler)
}

func (b *commandBus) start() {
	go func() {
		for cmdWithCtx := range b.queue {
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
