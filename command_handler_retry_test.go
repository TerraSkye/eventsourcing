package eventsourcing_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
)

// TestNewCommandHandler_RetryBudgetPerCommand covers a handler run by a
// CommandBus on several shards at once: every command gets a BackOff of its
// own, so with WithMaxRetries(..., 3) and a store that always conflicts
// each one is saved exactly 1+3 times, and -race sees no shared BackOff.
func TestNewCommandHandler_RetryBudgetPerCommand(t *testing.T) {
	store := &alwaysConflictStore{saves: map[string]int{}}
	handler := eventsourcing.NewCommandHandler(store,
		func() int { return 0 },
		func(s int, _ *eventsourcing.Envelope) int { return s },
		func(s int, c bumpCmd) ([]eventsourcing.Event, error) { return []eventsourcing.Event{bumped{c.ID}}, nil },
		eventsourcing.WithRetryStrategy(func() backoff.BackOff {
			return backoff.WithMaxRetries(backoff.NewConstantBackOff(2*time.Millisecond), 3)
		}),
	)
	bus := eventsourcing.NewCommandBus(16, 8)
	defer bus.Stop()
	eventsourcing.Register(bus, handler)

	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = bus.Dispatch(ctx, bumpCmd{ID: id})
		}(fmt.Sprintf("agg-%d", i))
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("dispatches did not finish")
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range n {
		id := fmt.Sprintf("agg-%d", i)
		if got := store.saves[id]; got != 4 {
			t.Errorf("stream %s: Save called %d times, want 4 (1 attempt + 3 retries)", id, got)
		}
	}
}

type bumpCmd struct{ ID string }

func (c bumpCmd) AggregateID() string { return c.ID }
func (c bumpCmd) CommandType() string { return "bump" }

type bumped struct{ ID string }

func (e bumped) AggregateID() string { return e.ID }
func (e bumped) EventType() string   { return "bumped" }

// alwaysConflictStore reports a revision conflict on every Save, counting
// the saves per stream. Loads return an empty stream.
type alwaysConflictStore struct {
	mu    sync.Mutex
	saves map[string]int
}

func (s *alwaysConflictStore) Save(_ context.Context, envs []eventsourcing.Envelope, rev eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	s.mu.Lock()
	s.saves[envs[0].StreamID]++
	s.mu.Unlock()
	return eventsourcing.AppendResult{}, &eventsourcing.StreamRevisionConflictError{Stream: envs[0].StreamID, ExpectedRevision: rev, ActualRevision: eventsourcing.Revision(1)}
}

func (s *alwaysConflictStore) LoadStream(ctx context.Context, id string) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return s.LoadStreamFrom(ctx, id, eventsourcing.Any{})
}

func (s *alwaysConflictStore) LoadStreamFrom(ctx context.Context, _ string, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator[*eventsourcing.Envelope](ctx, nil), nil
}

func (s *alwaysConflictStore) Close() error { return nil }

// TestNewCommandHandler_RetryWaitEndsWithContext covers a command whose ctx
// is cancelled while the handler waits to retry a save conflict: the wait
// ends at once, and the CommandBus shard that ran it is free for the next
// command.
func TestNewCommandHandler_RetryWaitEndsWithContext(t *testing.T) {
	newHandler := func(saves *atomic.Int32) eventsourcing.CommandHandler[retryCtxCmd] {
		store := &conflictOnceStore{saves: saves}
		return eventsourcing.NewCommandHandler(store,
			func() int { return 0 },
			func(s int, _ *eventsourcing.Envelope) int { return s + 1 },
			func(int, retryCtxCmd) ([]eventsourcing.Event, error) {
				return []eventsourcing.Event{retryCtxEvent{}}, nil
			},
			eventsourcing.WithRetryStrategy(func() backoff.BackOff { return backoff.NewConstantBackOff(2 * time.Second) }),
		)
	}

	t.Run("direct call returns promptly on cancel", func(t *testing.T) {
		var saves atomic.Int32
		h := newHandler(&saves)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)

		start := time.Now()
		_, err := h(ctx, retryCtxCmd{ID: "a"})
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("handler returned %v after ctx was cancelled at 50ms; want < 500ms", elapsed)
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v; want it to wrap context.Canceled", err)
		}
	})

	t.Run("cancelled command does not block its bus shard", func(t *testing.T) {
		var saves atomic.Int32
		bus := eventsourcing.NewCommandBus(4, 1)
		defer bus.Stop()
		eventsourcing.Register(bus, newHandler(&saves))

		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		_, _ = bus.Dispatch(ctx, retryCtxCmd{ID: "a"}) // conflicts, then waits

		start := time.Now()
		ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel2()
		if _, err := bus.Dispatch(ctx2, retryCtxCmd{ID: "a"}); err != nil {
			t.Fatalf("second dispatch: %v", err)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Errorf("next command on the shard waited %v for the cancelled one's retry wait; want < 500ms", elapsed)
		}
	})
}

type retryCtxCmd struct{ ID string }

func (c retryCtxCmd) AggregateID() string { return c.ID }
func (c retryCtxCmd) CommandType() string { return "retryCtxCmd" }

type retryCtxEvent struct{}

func (retryCtxEvent) EventType() string   { return "retryCtxEvent" }
func (retryCtxEvent) AggregateID() string { return "a" }

// conflictOnceStore reports a revision conflict on its first Save and
// accepts every later one. Loads return an empty stream bound to ctx.
type conflictOnceStore struct {
	eventsourcing.EventStore
	saves *atomic.Int32
}

func (s *conflictOnceStore) Save(_ context.Context, envs []eventsourcing.Envelope, rev eventsourcing.StreamState) (eventsourcing.AppendResult, error) {
	if s.saves.Add(1) == 1 {
		return eventsourcing.AppendResult{}, &eventsourcing.StreamRevisionConflictError{Stream: envs[0].StreamID, ExpectedRevision: rev, ActualRevision: eventsourcing.Revision(1)}
	}
	return eventsourcing.AppendResult{Successful: true, StreamID: envs[0].StreamID}, nil
}

func (s *conflictOnceStore) LoadStreamFrom(ctx context.Context, _ string, _ eventsourcing.StreamState) (*eventsourcing.Iterator[*eventsourcing.Envelope], error) {
	return eventsourcing.NewSliceIterator[*eventsourcing.Envelope](ctx, nil), nil
}
