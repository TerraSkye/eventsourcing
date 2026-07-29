//go:build integration

package kurrentdb

import (
	"context"
	"sync"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
)

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #30: Use
// appended to b.middlewares with no synchronization, and Subscribe read
// b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
//
// The bus is closed up front so Subscribe bails out right after building the
// wrapped handler (reading b.middlewares) and before it ever touches the nil
// KurrentDB client — this isolates the middlewares race from needing a live
// server.
func TestUse_RacesWithSubscribe(t *testing.T) {
	bus := NewEventBus(nil, 10)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			bus.Use(func(next cqrs.EventHandler) cqrs.EventHandler {
				return next
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = bus.Subscribe(context.Background(), "sub", handler)
		}
	}()

	wg.Wait()
}
