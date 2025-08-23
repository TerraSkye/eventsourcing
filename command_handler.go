package eventsourcing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type CommandHandler interface {
	Handle(ctx context.Context, command Command) error
}

type commandHandler struct {
	store               EventStore
	tracer              trace.Tracer
	aggregateForCommand func(cmd Command) (Aggregate, error)
	dispatchEvent       func(aggregate Aggregate, event Event) error
	dispatchCommand     func(ctx context.Context, aggregate Aggregate, command Command) error
}

// NewCommandHandler creates a new CommandHandler for an aggregate type.
func NewCommandHandler(
	store EventStore,
	aggregateForCommand func(cmd Command) (Aggregate, error),
	dispatchEvent func(aggregate Aggregate, event Event) error,
	dispatchCommand func(ctx context.Context, aggregate Aggregate, command Command) error,
) CommandHandler {

	tracer := otel.Tracer("command-handler")

	h := &commandHandler{
		store:               store,
		tracer:              tracer,
		aggregateForCommand: aggregateForCommand,
		dispatchEvent: func(aggregate Aggregate, event Event) error {
			err := dispatchEvent(aggregate, event)
			return err
		},
		dispatchCommand: func(ctx context.Context, aggregate Aggregate, command Command) error {
			ctx, span := tracer.Start(ctx, "cqrs.command.handler.apply_command",
				trace.WithAttributes(
					attribute.String("command.type", TypeName(command)),
					attribute.String("aggregate.id", command.AggregateID()),
					attribute.String("aggregate.version", MustExtractAggregateVersion(ctx)),

					attribute.String("cqrs.causation_id", MustExtractCausationId(ctx)),
					attribute.String("cqrs.correlation_id", trace.SpanContextFromContext(ctx).TraceID().String()),
					attribute.String("cqrs.command", TypeName(command)),
					// Messaging attributes
					attribute.String("messaging.conversation_id", trace.SpanContextFromContext(ctx).TraceID().String()),
					attribute.String("messaging.destination_kind", "aggregate"),
					attribute.String("messaging.message_id", MustExtractCausationId(ctx)),
					attribute.String("messaging.system", "cqrs"),
				),
			)

			defer span.End()

			err := dispatchCommand(ctx, aggregate, command)

			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			} else {
				span.SetStatus(codes.Ok, "")

			}
			return err

		},
	}

	return h
}

func (h *commandHandler) Handle(ctx context.Context, command Command) error {

	// Start the tracing span for handling the command
	ctx, span := h.tracer.Start(ctx, "cqrs.command.handler.execute",
		trace.WithAttributes(
			attribute.String("aggregate.id", command.AggregateID()),
			attribute.String("command.type", TypeName(command)),
		),
	)
	defer span.End()

	aggregate, err := h.aggregateForCommand(command)

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	var version uint64

	{

		ctx2, span2 := h.tracer.Start(ctx, "cqrs.command.handler.load_aggregate",
			trace.WithAttributes(
				attribute.String("aggregate.id", command.AggregateID()),
			),
		)

		// the aggregate is being prepared here. we would want to trace this as wel.

		events, err := h.store.LoadStream(ctx2, command.AggregateID())

		if err != nil {
			span2.RecordError(err)
			span2.SetStatus(codes.Error, err.Error())
			span2.End()
			return err
		}

		// hydrating the model the aggregate with events.

		for event := range events {
			if err := h.dispatchEvent(aggregate, event.Event); err != nil {
				span2.RecordError(err)
				span2.SetStatus(codes.Error, err.Error())
				span2.End()
				return err
			}
			version++
		}

		aggregate.SetAggregateVersion(version)

		span2.AddEvent("aggregate loaded", trace.WithAttributes(
			attribute.String("aggregate.id", command.AggregateID()),
			attribute.Int64("aggregate.version", int64(version)),
		))

		span2.SetStatus(codes.Ok, "aggregate loaded successfully")
		span2.End()
	}

	if err := h.dispatchCommand(WithAggregateID(WithAggregateVersion(ctx, version), command.AggregateID()), aggregate, command); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	//events
	uncomittedEvents := aggregate.UncommittedEvents()

	if err := h.store.Save(ctx, uncomittedEvents, version); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Mark success in tracing
	span.SetStatus(codes.Ok, "")

	return nil

}
