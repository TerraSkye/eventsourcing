package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	pgbus "github.com/terraskye/eventsourcing/eventbus/postgres"
)

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #35: Use
// appended to b.middlewares with no synchronization at all, and Subscribe
// read b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
func TestUse_RacesWithSubscribe(t *testing.T) {
	pool := newPool(t)
	bus := pgbus.NewEventBus(pool, 50*time.Millisecond)
	t.Cleanup(func() { _ = bus.Close() })

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			bus.Use(func(next cqrs.EventHandler) cqrs.EventHandler {
				return next
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			// Same name every time: only the first call actually registers a
			// subscriber, but every call reads b.middlewares before that
			// check, which is enough to race against the writer above.
			_ = bus.Subscribe(context.Background(), "sub", handler)
		}
	}()

	wg.Wait()
}
