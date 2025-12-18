package eventsourcing

import (
	"context"
	"errors"
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
