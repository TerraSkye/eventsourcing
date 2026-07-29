package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
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

// TestCommandBus_HandlerPanicWithError covers the branch where the recovered
// panic value already is an error (as opposed to TestCommandBus_HandlerPanic's
// string panic), so the worker uses it directly instead of wrapping it with
// fmt.Errorf("panic: %v", r).
func TestCommandBus_HandlerPanicWithError(t *testing.T) {
	bus := NewCommandBus(10, 1)

	boom := errors.New("boom")
	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		panic(boom)
	})

	_, err := bus.Dispatch(context.Background(), testCmd{ID: "x"})
	if err == nil {
		t.Fatalf("expected panic recovery error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected error chain to contain the panicked error, got: %v", err)
	}
	if !errors.Is(err, ErrHandlerPanicked) {
		t.Fatalf("expected error chain to contain ErrHandlerPanicked, got: %v", err)
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

// TestRegister_HandlerTypeMismatchReturnsError covers Register's generated
// wrapper's own type assertion (cmd.(C)) failing. In normal use this can't
// happen — the worker only ever looks a handler up by the same %T string
// Register derived it from — so this calls the wrapper directly (fetched via
// handlerFor, bypassing the worker's cmdName-matching) with a command of a
// different concrete type to exercise the defensive check itself.
func TestRegister_HandlerTypeMismatchReturnsError(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	cmdName := fmt.Sprintf("%T", testCmd{})
	h, ok := bus.handlerFor(cmdName)
	if !ok {
		t.Fatalf("expected a handler registered for %s", cmdName)
	}

	_, err := h(context.Background(), testCmd2{ID: "mismatched"})
	if err == nil {
		t.Fatalf("expected an error for a command type mismatch")
	}
	if !strings.Contains(err.Error(), "expected command type") {
		t.Fatalf("expected a type-mismatch error, got: %v", err)
	}

	bus.Stop()
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
			name: "a minimum of 1 shard is present",
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

type stopProbeCmd struct{ ID string }

func (c stopProbeCmd) AggregateID() string { return c.ID }

// TestCommandBus_StopDoesNotPanicPendingDispatch asserts that a Dispatch parked
// on the queue send when Stop runs is either processed or fails with
// ErrCommandBusClosed. It must never panic the sender, which is what closing the
// shard queues used to do.
func TestCommandBus_StopDoesNotPanicPendingDispatch(t *testing.T) {
	// Unbuffered queue, single shard: every Dispatch beyond the one the worker
	// picked up parks on the channel send.
	bus := NewCommandBus(0, 1)

	release := make(chan struct{})
	var once sync.Once
	Register(bus, func(ctx context.Context, cmd stopProbeCmd) (AppendResult, error) {
		<-release // occupy the single worker
		return AppendResult{Successful: true}, nil
	})

	const dispatchers = 32
	var wg sync.WaitGroup
	errCh := make(chan error, dispatchers)

	for i := 0; i < dispatchers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := bus.Dispatch(context.Background(), stopProbeCmd{ID: "agg-1"})
			errCh <- err
		}()
	}

	// Give the dispatchers time to block on the queue send.
	time.Sleep(100 * time.Millisecond)

	go func() {
		// Let Stop's wg.Wait() finish once it gets there.
		time.Sleep(200 * time.Millisecond)
		once.Do(func() { close(release) })
	}()

	bus.Stop() // must not panic the senders parked on the queue

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil && !errors.Is(err, ErrCommandBusClosed) {
			t.Fatalf("unexpected dispatch error: %v", err)
		}
	}
}

// TestCommandBus_StopIsIdempotent guards the sync.Once around close(stopCh); a
// second Stop used to panic with "close of closed channel".
func TestCommandBus_StopIsIdempotent(t *testing.T) {
	bus := NewCommandBus(10, 1)

	Register(bus, func(ctx context.Context, cmd testCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	bus.Stop()
	bus.Stop() // must not panic
}

// Command types used only by TestCommandBus_RegisterWhileDispatching. Each
// Register call writes a new key into bus.handlers, which is what used to race
// with the worker goroutine's read.
type raceCmdA struct{ ID string }

func (c raceCmdA) AggregateID() string { return c.ID }

type raceCmdB struct{ ID string }

func (c raceCmdB) AggregateID() string { return c.ID }

type raceCmdC struct{ ID string }

func (c raceCmdC) AggregateID() string { return c.ID }

type raceCmdD struct{ ID string }

func (c raceCmdD) AggregateID() string { return c.ID }

type raceCmdE struct{ ID string }

func (c raceCmdE) AggregateID() string { return c.ID }

// TestCommandBus_RegisterWhileDispatching asserts the concurrency contract the
// CommandBus godoc promises: the type carries "synchronization mechanisms for
// safe concurrent access" and Dispatch "is safe to call concurrently".
// Registering a handler on a live bus while commands are in flight must
// therefore be race-free. The worker's map read used to be unguarded, which
// aborted the process with "fatal error: concurrent map read and map write".
func TestCommandBus_RegisterWhileDispatching(t *testing.T) {
	for i := 0; i < 50; i++ {
		bus := NewCommandBus(64, 1)

		Register(bus, func(ctx context.Context, cmd raceCmdA) (AppendResult, error) {
			return AppendResult{Successful: true}, nil
		})

		// Keep the worker busy reading b.handlers, and signal once it is
		// actually draining the queue so the registrations below overlap it.
		var wg sync.WaitGroup
		running := make(chan struct{})
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			var once sync.Once
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := bus.Dispatch(context.Background(), raceCmdA{ID: "a"}); err != nil {
					return
				}
				once.Do(func() { close(running) })
			}
		}()

		<-running

		// Concurrently register further handlers, each writing b.handlers.
		Register(bus, func(ctx context.Context, cmd raceCmdB) (AppendResult, error) {
			return AppendResult{Successful: true}, nil
		})
		Register(bus, func(ctx context.Context, cmd raceCmdC) (AppendResult, error) {
			return AppendResult{Successful: true}, nil
		})
		Register(bus, func(ctx context.Context, cmd raceCmdD) (AppendResult, error) {
			return AppendResult{Successful: true}, nil
		})
		Register(bus, func(ctx context.Context, cmd raceCmdE) (AppendResult, error) {
			return AppendResult{Successful: true}, nil
		})

		close(stop)
		wg.Wait()
		bus.Stop()
	}
}

type benchCmd struct{ ID string }

func (c benchCmd) AggregateID() string { return c.ID }

// benchmarkDispatch measures end-to-end Dispatch throughput. Dispatch is
// synchronous, so this covers the whole round trip: the stopCh check and wg.Add
// under b.mu, the queue send, worker pickup, the handler, and the response
// receive.
func benchmarkDispatch(b *testing.B, shards, parallelism int) {
	bus := NewCommandBus(1024, shards)
	defer bus.Stop()

	Register(bus, func(ctx context.Context, cmd benchCmd) (AppendResult, error) {
		return AppendResult{Successful: true}, nil
	})

	ids := make([]string, 512)
	for i := range ids {
		ids[i] = "agg-" + strconv.Itoa(i)
	}

	ctx := context.Background()
	if parallelism > 0 {
		b.SetParallelism(parallelism)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := bus.Dispatch(ctx, benchCmd{ID: ids[i%len(ids)]}); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkCommandBusDispatchShards1(b *testing.B)  { benchmarkDispatch(b, 1, 0) }
func BenchmarkCommandBusDispatchShards8(b *testing.B)  { benchmarkDispatch(b, 8, 0) }
func BenchmarkCommandBusDispatchShards32(b *testing.B) { benchmarkDispatch(b, 32, 0) }

// BenchmarkCommandBusDispatchContended oversubscribes GOMAXPROCS 32x so every
// dispatcher contends for the b.mu that orders wg.Add against Stop's wg.Wait.
func BenchmarkCommandBusDispatchContended(b *testing.B) { benchmarkDispatch(b, 16, 32) }
