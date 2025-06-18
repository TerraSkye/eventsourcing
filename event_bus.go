package eventsourcing

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"sync"
)

type EventBus interface {
	Dispatch(ctx context.Context, event Event) error
	Subscribe(handler EventHandler)
	SubscribeToGroup(handler *EventGroupProcessor)
}

type eventBus struct {
	tracer        trace.Tracer
	handlers      []EventHandler
	groupHandlers map[string][]GroupEventHandler
	totalHandlers uint64
	sync.RWMutex
}

func NewEventBus() EventBus {
	return &eventBus{
		tracer:        otel.Tracer("eventbus"),
		groupHandlers: make(map[string][]GroupEventHandler),
		handlers:      make([]EventHandler, 0),
	}
}

// Dispatch sends the event to all subscribed handlers concurrently.
func (b *eventBus) Dispatch(ctx context.Context, event Event) error {
	// Start a new tracing span, linking it to the incoming context
	ctx, span := b.tracer.Start(ctx, "cqrs.event.bus.dispatch",
		trace.WithAttributes(
			attribute.String("event.aggregate_id", event.AggregateID()),
			attribute.String("event.type", TypeName(event)),
		),
	)
	defer span.End()

	b.RLock()
	handlers := append([]EventHandler{}, b.handlers...)
	b.RUnlock()

	var wg sync.WaitGroup
	errChan := make(chan error, len(handlers))

	for _, handler := range handlers {
		expectedEvent := handler.NewEvent()

		if TypeName(expectedEvent) == TypeName(event) {
			wg.Add(1)
			go func(h EventHandler) {
				defer wg.Done()
				h.HandlerName()
				// Create a new span for the handler, linking it to Dispatch
				handlerCtx, handlerSpan := b.tracer.Start(ctx, "cqrs.event.handler.process",
					trace.WithAttributes(
						attribute.String("handler.name", h.HandlerName()),
						attribute.String("event.aggregate_id", event.AggregateID()),
						attribute.String("event.type", TypeName(event)),
					),
					trace.WithLinks(trace.LinkFromContext(ctx)),
				)

				if err := h.Handle(handlerCtx, event); err != nil {
					handlerSpan.RecordError(err)
					handlerSpan.SetStatus(codes.Error, err.Error())
					errChan <- err
				}

				handlerSpan.End()
			}(handler)
		}
	}

	for handlerName, group := range b.groupHandlers {

		for _, handler := range group {
			expectedEvent := handler.NewEvent()
			if TypeName(expectedEvent) == TypeName(event) {
				wg.Add(1)
				go func(h GroupEventHandler) {
					defer wg.Done()

					// Create a new span for the handler, linking it to Dispatch
					handlerCtx, handlerSpan := b.tracer.Start(ctx, "cqrs.event.handler.process",
						trace.WithAttributes(
							attribute.String("handler.name", handlerName),
							attribute.String("event.aggregate_id", event.AggregateID()),
							attribute.String("event.type", TypeName(event)),
						),
						trace.WithLinks(trace.LinkFromContext(ctx)),
					)

					if err := h.Handle(handlerCtx, event); err != nil {
						handlerSpan.RecordError(err)
						handlerSpan.SetStatus(codes.Error, err.Error())
						errChan <- err
					}
					handlerSpan.End()
				}(handler)
			}
		}
	}

	wg.Wait()
	close(errChan)

	if len(errChan) > 0 {

		err := <-errChan
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}

func (b *eventBus) Subscribe(handler EventHandler) {
	b.Lock()
	defer b.Unlock()
	b.handlers = append(b.handlers, handler)
}

func (b *eventBus) SubscribeToGroup(handler *EventGroupProcessor) {
	b.Lock()
	defer b.Unlock()
	b.groupHandlers[handler.groupName] = handler.groupEventHandlers
	b.totalHandlers += uint64(len(handler.groupEventHandlers))
}
