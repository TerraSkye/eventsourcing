package eventsourcing

import (
	"context"
	"errors"
	"strconv"
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
