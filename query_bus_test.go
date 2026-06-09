package eventsourcing

import (
	"context"
	"strings"
	"testing"
)

type ListTasksQuery struct {
	Owner string
}

func (q ListTasksQuery) ID() []byte { return []byte(q.Owner) }

type TaskListResult struct {
	Tasks []string
}

func TestQueryBus_RegisterAndLookup(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "found"}, nil
	}))

	if len(bus.handlers) != 1 {
		t.Errorf("len(bus.handlers) = %d, want 1", len(bus.handlers))
	}
}

func TestQueryBus_MultipleHandlers(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "single"}, nil
	}))

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ListTasksQuery) (*TaskListResult, error) {
		return &TaskListResult{Tasks: []string{"a", "b"}}, nil
	}))

	if len(bus.handlers) != 2 {
		t.Errorf("len(bus.handlers) = %d, want 2", len(bus.handlers))
	}
}

// ---- Middleware tests ----

type mwTestQueryMiddleware struct {
	timesCalled uint
}

func (m *mwTestQueryMiddleware) Middleware(
	next func(ctx context.Context, qry any) (any, error),
) func(ctx context.Context, qry any) (any, error) {
	return func(ctx context.Context, qry any) (any, error) {
		m.timesCalled++
		return next(ctx, qry)
	}
}

func TestQueryBusMiddlewareAdd(t *testing.T) {
	bus := NewQueryBus()
	mw := &mwTestQueryMiddleware{}

	bus.useMiddleware(mw)
	if len(bus.middlewares) != 1 {
		t.Fatalf("expected 1 middleware after useMiddleware, got %d", len(bus.middlewares))
	}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 2 {
		t.Fatalf("expected 2 middlewares after Use(method), got %d", len(bus.middlewares))
	}

	noop := QueryBusMiddleware(func(next func(ctx context.Context, qry any) (any, error)) func(ctx context.Context, qry any) (any, error) {
		return next
	})
	bus.Use(noop)
	if len(bus.middlewares) != 3 {
		t.Fatalf("expected 3 middlewares after Use(func literal), got %d", len(bus.middlewares))
	}
}

func TestQueryBusMiddleware(t *testing.T) {
	bus := NewQueryBus()
	mw := &mwTestQueryMiddleware{}
	bus.useMiddleware(mw)

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

	t.Run("Use re-wraps already-registered handlers", func(t *testing.T) {
		bus.Use(mw.Middleware)

		if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "2"}); err != nil {
			t.Fatal(err)
		}
		// struct mw + func mw both run: +2 → total 3
		if mw.timesCalled != 3 {
			t.Fatalf("expected 3 total calls, got %d", mw.timesCalled)
		}
	})
}

func TestQueryBusMiddlewareExecution(t *testing.T) {
	bus := NewQueryBus()
	var out []string

	bus.Use(QueryBusMiddleware(func(next func(ctx context.Context, qry any) (any, error)) func(ctx context.Context, qry any) (any, error) {
		return func(ctx context.Context, qry any) (any, error) {
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

func TestQueryBusMiddlewareOrder(t *testing.T) {
	bus := NewQueryBus()
	var order []string

	bus.Use(
		QueryBusMiddleware(func(next func(ctx context.Context, qry any) (any, error)) func(ctx context.Context, qry any) (any, error) {
			return func(ctx context.Context, qry any) (any, error) {
				order = append(order, "first")
				return next(ctx, qry)
			}
		}),
		QueryBusMiddleware(func(next func(ctx context.Context, qry any) (any, error)) func(ctx context.Context, qry any) (any, error) {
			return func(ctx context.Context, qry any) (any, error) {
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

func TestQueryBusMiddlewareAppliedRetroactively(t *testing.T) {
	bus := NewQueryBus()
	mw := &mwTestQueryMiddleware{}

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "ok"}, nil
	}))

	bus.useMiddleware(mw)

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "x"}); err != nil {
		t.Fatal(err)
	}

	if mw.timesCalled != 1 {
		t.Fatalf("expected 1 call, got %d", mw.timesCalled)
	}
}

func TestQueryBusFuncAndStructMiddleware(t *testing.T) {
	bus := NewQueryBus()
	structMw := &mwTestQueryMiddleware{}
	funcCalls := 0

	bus.useMiddleware(structMw)
	bus.Use(QueryBusMiddleware(func(next func(ctx context.Context, qry any) (any, error)) func(ctx context.Context, qry any) (any, error) {
		return func(ctx context.Context, qry any) (any, error) {
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
