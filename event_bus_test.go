package eventsourcing

import (
	"context"
	"strings"
	"testing"
)

// ---- Stubs ----

type mwTestEventMiddleware struct {
	timesCalled uint
}

func (m *mwTestEventMiddleware) Middleware(next EventHandler) EventHandler {
	return NewEventHandlerFunc(func(ctx context.Context, event Event) error {
		m.timesCalled++
		return next.Handle(ctx, event)
	})
}

type mwMockEventBus struct {
	subscriptions map[string]EventHandler
}

func newMwMockEventBus() *mwMockEventBus {
	return &mwMockEventBus{subscriptions: make(map[string]EventHandler)}
}

func (b *mwMockEventBus) Subscribe(_ context.Context, name string, h EventHandler, _ ...SubscriberOption) error {
	b.subscriptions[name] = h
	return nil
}

func (b *mwMockEventBus) dispatch(ctx context.Context, ev Event) error {
	for _, h := range b.subscriptions {
		if err := h.Handle(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

func (b *mwMockEventBus) Errors() <-chan error { return make(chan error) }
func (b *mwMockEventBus) Close() error         { return nil }

type mwTestBusEvent struct{}

func (e *mwTestBusEvent) AggregateID() string { return "test" }
func (e *mwTestBusEvent) EventType() string   { return "mwTestBusEvent" }

// ---- Tests ----

func TestEventBusMiddlewareAdd(t *testing.T) {
	inner := newMwMockEventBus()
	mw := &mwTestEventMiddleware{}

	bus := NewEventBusWithMiddleware(inner, mw.Middleware)
	mb := bus.(*middlewareEventBus)

	if len(mb.middlewares) != 1 {
		t.Fatalf("expected 1 middleware after NewEventBusWithMiddleware, got %d", len(mb.middlewares))
	}

	mb.useMiddleware(mw)
	if len(mb.middlewares) != 2 {
		t.Fatalf("expected 2 middlewares after useMiddleware, got %d", len(mb.middlewares))
	}

	mb.useMiddleware(EventHandlerMiddleware(func(next EventHandler) EventHandler { return next }))
	if len(mb.middlewares) != 3 {
		t.Fatalf("expected 3 middlewares after second useMiddleware, got %d", len(mb.middlewares))
	}
}

func TestEventBusMiddleware(t *testing.T) {
	inner := newMwMockEventBus()
	mw := &mwTestEventMiddleware{}

	bus := NewEventBusWithMiddleware(inner, mw.Middleware)

	handlerCalled := false
	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		handlerCalled = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	t.Run("middleware and handler called on dispatch", func(t *testing.T) {
		if err := inner.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
			t.Fatal(err)
		}
		if !handlerCalled {
			t.Fatal("handler should have been called")
		}
		if mw.timesCalled != 1 {
			t.Fatalf("expected 1 middleware call, got %d", mw.timesCalled)
		}
	})

	t.Run("middleware called again on second event", func(t *testing.T) {
		if err := inner.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
			t.Fatal(err)
		}
		if mw.timesCalled != 2 {
			t.Fatalf("expected 2 total middleware calls, got %d", mw.timesCalled)
		}
	})
}

func TestEventBusMiddlewareExecution(t *testing.T) {
	inner := newMwMockEventBus()
	var out []string

	bus := NewEventBusWithMiddleware(inner,
		EventHandlerMiddleware(func(next EventHandler) EventHandler {
			return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
				out = append(out, "middleware")
				return next.Handle(ctx, ev)
			})
		}),
	)

	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		out = append(out, "handler")
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := inner.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	want := "middleware,handler"
	if strings.Join(out, ",") != want {
		t.Fatalf("expected %q, got %q", want, strings.Join(out, ","))
	}
}

func TestEventBusMiddlewareOrder(t *testing.T) {
	inner := newMwMockEventBus()
	var order []string

	bus := NewEventBusWithMiddleware(inner,
		EventHandlerMiddleware(func(next EventHandler) EventHandler {
			return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
				order = append(order, "first")
				return next.Handle(ctx, ev)
			})
		}),
		EventHandlerMiddleware(func(next EventHandler) EventHandler {
			return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
				order = append(order, "second")
				return next.Handle(ctx, ev)
			})
		}),
	)

	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		order = append(order, "handler")
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := inner.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, order)
	}
}

func TestEventBusFuncAndStructMiddleware(t *testing.T) {
	inner := newMwMockEventBus()
	structMw := &mwTestEventMiddleware{}
	funcCalls := 0

	bus := NewEventBusWithMiddleware(inner, structMw.Middleware)
	mb := bus.(*middlewareEventBus)
	mb.useMiddleware(EventHandlerMiddleware(func(next EventHandler) EventHandler {
		return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
			funcCalls++
			return next.Handle(ctx, ev)
		})
	}))

	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := inner.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	if structMw.timesCalled != 1 {
		t.Fatalf("struct middleware: expected 1 call, got %d", structMw.timesCalled)
	}
	if funcCalls != 1 {
		t.Fatalf("func middleware: expected 1 call, got %d", funcCalls)
	}
}
