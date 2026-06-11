package eventsourcing

import (
	"context"
	"strings"
	"testing"
)

// ---- Command middleware stubs ----

type mwTestCommandMiddleware struct {
	timesCalled uint
}

func (m *mwTestCommandMiddleware) Middleware(next CommandHandler[Command]) CommandHandler[Command] {
	return func(ctx context.Context, cmd Command) (AppendResult, error) {
		m.timesCalled++
		return next(ctx, cmd)
	}
}

// ---- Command middleware tests ----

func TestCommandBusMiddlewareAdd(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	mw := &mwTestCommandMiddleware{}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 1 {
		t.Fatalf("expected 1 middleware after Use, got %d", len(bus.middlewares))
	}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 2 {
		t.Fatalf("expected 2 middlewares after Use(method), got %d", len(bus.middlewares))
	}

	noop := CommandHandlerMiddleware(func(next CommandHandler[Command]) CommandHandler[Command] {
		return next
	})
	bus.Use(noop)
	if len(bus.middlewares) != 3 {
		t.Fatalf("expected 3 middlewares after Use(func literal), got %d", len(bus.middlewares))
	}
}

func TestCommandBusMiddleware(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	mw := &mwTestCommandMiddleware{}
	bus.Use(mw.Middleware)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	t.Run("called on successful dispatch", func(t *testing.T) {
		if _, err := bus.Dispatch(context.Background(), testCmd{ID: "a"}); err != nil {
			t.Fatal(err)
		}
		if mw.timesCalled != 1 {
			t.Fatalf("expected 1 call, got %d", mw.timesCalled)
		}
	})

	t.Run("not called for unregistered command type", func(t *testing.T) {
		if _, err := bus.Dispatch(context.Background(), testCmd2{ID: "b"}); err == nil {
			t.Fatal("expected error for missing handler")
		}
		if mw.timesCalled != 1 {
			t.Fatalf("middleware should not be called for unregistered type; still %d", mw.timesCalled)
		}
	})
}

func TestCommandBusMiddlewareExecution(t *testing.T) {
	mwLabel := "middleware"
	handlerLabel := "handler"

	t.Run("handler called without middleware", func(t *testing.T) {
		bus := NewCommandBus(10, 1)
		defer bus.Stop()

		var out []string
		Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
			out = append(out, handlerLabel)
			return AppendResult{Successful: true}, nil
		})

		if _, err := bus.Dispatch(context.Background(), testCmd{ID: "x"}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(out, ",") != handlerLabel {
			t.Fatalf("unexpected output: %v", out)
		}
	})

	t.Run("middleware output precedes handler output", func(t *testing.T) {
		bus := NewCommandBus(10, 1)
		defer bus.Stop()

		var out []string
		bus.Use(func(next CommandHandler[Command]) CommandHandler[Command] {
			return func(ctx context.Context, cmd Command) (AppendResult, error) {
				out = append(out, mwLabel)
				return next(ctx, cmd)
			}
		})

		Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
			out = append(out, handlerLabel)
			return AppendResult{Successful: true}, nil
		})

		if _, err := bus.Dispatch(context.Background(), testCmd{ID: "x"}); err != nil {
			t.Fatal(err)
		}
		want := strings.Join([]string{mwLabel, handlerLabel}, ",")
		if strings.Join(out, ",") != want {
			t.Fatalf("expected %q, got %q", want, strings.Join(out, ","))
		}
	})
}

func TestCommandBusMiddlewareOrder(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	var order []string

	bus.Use(
		func(next CommandHandler[Command]) CommandHandler[Command] {
			return func(ctx context.Context, cmd Command) (AppendResult, error) {
				order = append(order, "first")
				return next(ctx, cmd)
			}
		},
		func(next CommandHandler[Command]) CommandHandler[Command] {
			return func(ctx context.Context, cmd Command) (AppendResult, error) {
				order = append(order, "second")
				return next(ctx, cmd)
			}
		},
	)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		order = append(order, "handler")
		return AppendResult{Successful: true}, nil
	})

	if _, err := bus.Dispatch(context.Background(), testCmd{ID: "x"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, order)
	}
}

func TestCommandBusMiddlewareAppliedAtRegister(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	mw := &mwTestCommandMiddleware{}
	bus.Use(mw.Middleware)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	if _, err := bus.Dispatch(context.Background(), testCmd{ID: "x"}); err != nil {
		t.Fatal(err)
	}

	if mw.timesCalled != 1 {
		t.Fatalf("expected 1 call, got %d", mw.timesCalled)
	}
}

func TestCommandBusFuncAndStructMiddleware(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	structMw := &mwTestCommandMiddleware{}
	funcCalls := 0

	bus.Use(structMw.Middleware)
	bus.Use(func(next CommandHandler[Command]) CommandHandler[Command] {
		return func(ctx context.Context, cmd Command) (AppendResult, error) {
			funcCalls++
			return next(ctx, cmd)
		}
	})

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	if _, err := bus.Dispatch(context.Background(), testCmd{ID: "x"}); err != nil {
		t.Fatal(err)
	}

	if structMw.timesCalled != 1 {
		t.Fatalf("struct middleware: expected 1 call, got %d", structMw.timesCalled)
	}
	if funcCalls != 1 {
		t.Fatalf("func middleware: expected 1 call, got %d", funcCalls)
	}
}

// ---- Query middleware stubs ----

type mwTestQueryMiddleware struct {
	timesCalled uint
}

func (m *mwTestQueryMiddleware) Middleware(next QueryGateway[Query, any]) QueryGateway[Query, any] {
	return func(ctx context.Context, qry Query) (any, error) {
		m.timesCalled++
		return next(ctx, qry)
	}
}

// ---- Query middleware tests ----

func TestQueryHandlerMiddlewareAdd(t *testing.T) {
	bus := NewQueryBus()
	mw := &mwTestQueryMiddleware{}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 1 {
		t.Fatalf("expected 1 middleware after Use, got %d", len(bus.middlewares))
	}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 2 {
		t.Fatalf("expected 2 middlewares after Use(method), got %d", len(bus.middlewares))
	}

	noop := QueryHandlerMiddleware(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
		return next
	})
	bus.Use(noop)
	if len(bus.middlewares) != 3 {
		t.Fatalf("expected 3 middlewares after Use(func literal), got %d", len(bus.middlewares))
	}
}

func TestQueryHandlerMiddleware(t *testing.T) {
	bus := NewQueryBus()
	mw := &mwTestQueryMiddleware{}
	bus.Use(mw.Middleware)

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "ok"}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

	t.Run("called on successful dispatch", func(t *testing.T) {
		if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"}); err != nil {
			t.Fatal(err)
		}
		if mw.timesCalled != 1 {
			t.Fatalf("expected 1 call, got %d", mw.timesCalled)
		}
	})

}

func TestQueryHandlerMiddlewareExecution(t *testing.T) {
	bus := NewQueryBus()
	var out []string

	bus.Use(QueryHandlerMiddleware(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
		return func(ctx context.Context, qry Query) (any, error) {
			out = append(out, "middleware")
			return next(ctx, qry)
		}
	}))

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		out = append(out, "handler")
		return &TaskResult{Title: "ok"}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "x"}); err != nil {
		t.Fatal(err)
	}

	want := "middleware,handler"
	if strings.Join(out, ",") != want {
		t.Fatalf("expected %q, got %q", want, strings.Join(out, ","))
	}
}

func TestQueryHandlerMiddlewareOrder(t *testing.T) {
	bus := NewQueryBus()
	var order []string

	bus.Use(
		QueryHandlerMiddleware(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
			return func(ctx context.Context, qry Query) (any, error) {
				order = append(order, "first")
				return next(ctx, qry)
			}
		}),
		QueryHandlerMiddleware(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
			return func(ctx context.Context, qry Query) (any, error) {
				order = append(order, "second")
				return next(ctx, qry)
			}
		}),
	)

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		order = append(order, "handler")
		return &TaskResult{Title: "ok"}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "x"}); err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, order)
	}
}

func TestQueryBusFuncAndStructMiddleware(t *testing.T) {
	bus := NewQueryBus()
	structMw := &mwTestQueryMiddleware{}
	funcCalls := 0

	bus.Use(structMw.Middleware)
	bus.Use(QueryHandlerMiddleware(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
		return func(ctx context.Context, qry Query) (any, error) {
			funcCalls++
			return next(ctx, qry)
		}
	}))

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "ok"}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "x"}); err != nil {
		t.Fatal(err)
	}

	if structMw.timesCalled != 1 {
		t.Fatalf("struct middleware: expected 1 call, got %d", structMw.timesCalled)
	}
	if funcCalls != 1 {
		t.Fatalf("func middleware: expected 1 call, got %d", funcCalls)
	}
}

// ---- Event bus middleware stubs ----

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
	middlewares   []EventHandlerMiddleware
}

func newMwMockEventBus() *mwMockEventBus {
	return &mwMockEventBus{subscriptions: make(map[string]EventHandler)}
}

func (b *mwMockEventBus) Use(middlewares ...EventHandlerMiddleware) {
	b.middlewares = append(b.middlewares, middlewares...)
}

func (b *mwMockEventBus) Subscribe(_ context.Context, name string, h EventHandler, _ ...SubscriberOption) error {
	wrapped := h
	for i := len(b.middlewares) - 1; i >= 0; i-- {
		wrapped = b.middlewares[i](wrapped)
	}
	b.subscriptions[name] = wrapped
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

// ---- Event bus middleware tests ----

func TestEventBusMiddlewareAdd(t *testing.T) {
	bus := newMwMockEventBus()
	mw := &mwTestEventMiddleware{}
	noop := EventHandlerMiddleware(func(next EventHandler) EventHandler { return next })

	bus.Use(mw.Middleware, mw.Middleware, noop)

	if len(bus.middlewares) != 3 {
		t.Fatalf("expected 3 middlewares, got %d", len(bus.middlewares))
	}
}

func TestEventBusMiddleware(t *testing.T) {
	bus := newMwMockEventBus()
	mw := &mwTestEventMiddleware{}

	bus.Use(mw.Middleware)

	handlerCalled := false
	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		handlerCalled = true
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	t.Run("middleware and handler called on dispatch", func(t *testing.T) {
		if err := bus.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
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
		if err := bus.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
			t.Fatal(err)
		}
		if mw.timesCalled != 2 {
			t.Fatalf("expected 2 total middleware calls, got %d", mw.timesCalled)
		}
	})
}

func TestEventBusMiddlewareExecution(t *testing.T) {
	bus := newMwMockEventBus()
	var out []string

	bus.Use(EventHandlerMiddleware(func(next EventHandler) EventHandler {
		return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
			out = append(out, "middleware")
			return next.Handle(ctx, ev)
		})
	}))

	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		out = append(out, "handler")
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := bus.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	want := "middleware,handler"
	if strings.Join(out, ",") != want {
		t.Fatalf("expected %q, got %q", want, strings.Join(out, ","))
	}
}

func TestEventBusMiddlewareOrder(t *testing.T) {
	bus := newMwMockEventBus()
	var order []string

	bus.Use(
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

	if err := bus.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "handler"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, order)
	}
}

func TestEventBusFuncAndStructMiddleware(t *testing.T) {
	bus := newMwMockEventBus()
	structMw := &mwTestEventMiddleware{}
	funcCalls := 0

	bus.Use(
		structMw.Middleware,
		func(next EventHandler) EventHandler {
			return NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
				funcCalls++
				return next.Handle(ctx, ev)
			})
		},
	)

	if err := bus.Subscribe(context.Background(), "sub", NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		return nil
	})); err != nil {
		t.Fatal(err)
	}

	if err := bus.dispatch(context.Background(), &mwTestBusEvent{}); err != nil {
		t.Fatal(err)
	}

	if structMw.timesCalled != 1 {
		t.Fatalf("struct middleware: expected 1 call, got %d", structMw.timesCalled)
	}
	if funcCalls != 1 {
		t.Fatalf("func middleware: expected 1 call, got %d", funcCalls)
	}
}
