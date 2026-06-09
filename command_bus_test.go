package eventsourcing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// ---- Test Stubs ----

type testCmd struct {
	ID string
}

func (c testCmd) AggregateID() string { return c.ID }

type testCmd2 struct {
	ID string
}

func (c testCmd2) AggregateID() string { return c.ID }

// ---- Tests ----

func TestCommandBus_Success(t *testing.T) {
	bus := NewCommandBus(10, 2)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	ctx := context.Background()
	res, err := bus.Dispatch(ctx, testCmd{ID: "abc"})

	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !res.Successful {
		t.Fatalf("expected successful result")
	}

	bus.Stop()
}

func TestCommandBus_NoHandler(t *testing.T) {
	bus := NewCommandBus(10, 1)

	_, err := bus.Dispatch(context.Background(), testCmd{ID: "missing"})

	if err == nil || err.Error() == "" {
		t.Fatalf("expected error for missing handler")
	}

	bus.Stop()
}

func TestCommandBus_HandlerPanic(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		panic("boom")
	})

	_, err := bus.Dispatch(context.Background(), testCmd{ID: "x"})

	if err == nil || err.Error() == "" {
		t.Fatalf("expected panic recovery error")
	}

	bus.Stop()
}

func TestCommandBus_ContextCancelBeforeEnqueue(t *testing.T) {
	bus := NewCommandBus(0, 1) // zero buffer so enqueue blocks

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	// Cancel immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := bus.Dispatch(ctx, testCmd{ID: "slow"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	bus.Stop()
}

func TestCommandBus_ContextCancelWhileWaiting(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		time.Sleep(200 * time.Millisecond)
		return AppendResult{Successful: true}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := bus.Dispatch(ctx, testCmd{ID: "slow-op"})
	if err == nil {
		t.Fatalf("expected timeout error")
	}

	bus.Stop()
}

func TestRegister_DuplicateHandlerPanics(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate handler")
		}
	}()

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})
}

func TestCommandBus_Stop(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	// Dispatch something before stopping
	_, err := bus.Dispatch(context.Background(), testCmd{ID: "x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	bus.Stop()

	// Now dispatch must fail
	_, err = bus.Dispatch(context.Background(), testCmd{ID: "x"})
	if err == nil {
		t.Fatalf("expected error after Stop")
	}
}

func TestCommandBus_ShardDeterministic(t *testing.T) {
	bus := NewCommandBus(10, 3)

	s1 := bus.selectShard("abc")
	s2 := bus.selectShard("abc")
	s3 := bus.selectShard("xyz")

	if s1 != s2 {
		t.Fatalf("shard hashing not deterministic")
	}
	if s1 == s3 {
		t.Fatalf("different IDs should likely map to different shards")
	}

	bus.Stop()
}

func TestNewCommandBus(t *testing.T) {
	type args struct {
		bufferSize int
		shardCount int
	}
	tests := []struct {
		name string
		args args
		want int
	}{
		{
			name: "a minumum of 1 shard is present",
			args: args{
				bufferSize: 1,
				shardCount: 0,
			},
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewCommandBus(tt.args.bufferSize, tt.args.shardCount).shardCount
			if got != tt.want {
				t.Errorf("NewCommandBus(%v, %v).shardCount = %v, want %v", tt.args.bufferSize, tt.args.shardCount, got, tt.want)
			}
		})
	}
}

// ---- Middleware tests ----

type mwTestCommandMiddleware struct {
	timesCalled uint
}

func (m *mwTestCommandMiddleware) Middleware(
	next func(ctx context.Context, cmd Command) (AppendResult, error),
) func(ctx context.Context, cmd Command) (AppendResult, error) {
	return func(ctx context.Context, cmd Command) (AppendResult, error) {
		m.timesCalled++
		return next(ctx, cmd)
	}
}

func TestCommandBusMiddlewareAdd(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	mw := &mwTestCommandMiddleware{}

	bus.useMiddleware(mw)
	if len(bus.middlewares) != 1 {
		t.Fatalf("expected 1 middleware after useMiddleware, got %d", len(bus.middlewares))
	}

	bus.Use(mw.Middleware)
	if len(bus.middlewares) != 2 {
		t.Fatalf("expected 2 middlewares after Use(method), got %d", len(bus.middlewares))
	}

	noop := CommandBusMiddleware(func(next func(ctx context.Context, cmd Command) (AppendResult, error)) func(ctx context.Context, cmd Command) (AppendResult, error) {
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
	bus.useMiddleware(mw)

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

	t.Run("Use re-wraps already-registered handlers", func(t *testing.T) {
		bus.Use(mw.Middleware)

		if _, err := bus.Dispatch(context.Background(), testCmd{ID: "c"}); err != nil {
			t.Fatal(err)
		}
		// struct mw + func mw both run: +2 calls → total 3
		if mw.timesCalled != 3 {
			t.Fatalf("expected 3 total calls, got %d", mw.timesCalled)
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
		bus.Use(func(next func(ctx context.Context, cmd Command) (AppendResult, error)) func(ctx context.Context, cmd Command) (AppendResult, error) {
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
		func(next func(ctx context.Context, cmd Command) (AppendResult, error)) func(ctx context.Context, cmd Command) (AppendResult, error) {
			return func(ctx context.Context, cmd Command) (AppendResult, error) {
				order = append(order, "first")
				return next(ctx, cmd)
			}
		},
		func(next func(ctx context.Context, cmd Command) (AppendResult, error)) func(ctx context.Context, cmd Command) (AppendResult, error) {
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

func TestCommandBusMiddlewareAppliedRetroactively(t *testing.T) {
	bus := NewCommandBus(10, 1)
	defer bus.Stop()

	mw := &mwTestCommandMiddleware{}

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	bus.useMiddleware(mw)

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

	bus.useMiddleware(structMw)
	bus.Use(func(next func(ctx context.Context, cmd Command) (AppendResult, error)) func(ctx context.Context, cmd Command) (AppendResult, error) {
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
