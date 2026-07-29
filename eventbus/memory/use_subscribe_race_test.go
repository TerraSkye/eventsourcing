package memory_test

import (
	"context"
	"sync"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventbus/memory"
)

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #33: Use
// appended to b.middlewares with no synchronization at all, and Subscribe
// read b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
func TestUse_RacesWithSubscribe(t *testing.T) {
	bus := memory.NewEventBus(1)

	noopMiddleware := func(next cqrs.EventHandler) cqrs.EventHandler {
		return next
	}
	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		bus.Use(noopMiddleware)
	}()

	go func() {
		defer wg.Done()
		_ = bus.Subscribe(context.Background(), "sub-1", handler)
	}()

	wg.Wait()
}
