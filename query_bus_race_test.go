package eventsourcing

import (
	"context"
	"sync"
	"testing"
)

// Query types used only by these tests. Each RegisterQueryHandler call writes a
// new key into bus.handlers under bus.mu; the gateway closure, NewQueryGateway,
// and Validate must all honor the same lock.
type raceQryA struct{ ID_ string }

func (q raceQryA) ID() []byte { return []byte(q.ID_) }

type raceQryB struct{ ID_ string }

func (q raceQryB) ID() []byte { return []byte(q.ID_) }

type raceQryC struct{ ID_ string }

func (q raceQryC) ID() []byte { return []byte(q.ID_) }

type raceQryD struct{ ID_ string }

func (q raceQryD) ID() []byte { return []byte(q.ID_) }

type raceQryE struct{ ID_ string }

func (q raceQryE) ID() []byte { return []byte(q.ID_) }

// Concrete pointer result type, deliberately NOT an interface, so these tests
// stay independent of the separately filed interface-result key collision.
type raceQryResult struct{ Value int }

// TestQueryBus_GatewayCallWhileRegistering is a regression test for GitHub
// issue #52: invoking a QueryGateway while another handler is registered on
// the same bus raced the gateway closure's unlocked read of bus.handlers
// against RegisterQueryHandler's locked write.
func TestQueryBus_GatewayCallWhileRegistering(t *testing.T) {
	for i := 0; i < 50; i++ {
		bus := NewQueryBus()

		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryA) (*raceQryResult, error) {
			return &raceQryResult{Value: 1}, nil
		})

		gateway := NewQueryGateway[raceQryA, *raceQryResult](bus)

		// Keep the gateway busy reading bus.handlers, and signal once it is
		// actually serving so the registrations below overlap it.
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
				if _, err := gateway(context.Background(), raceQryA{ID_: "a"}); err != nil {
					return
				}
				once.Do(func() { close(running) })
			}
		}()

		<-running

		// Concurrently register further handlers, each writing bus.handlers.
		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryB) (*raceQryResult, error) {
			return &raceQryResult{Value: 2}, nil
		})
		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryC) (*raceQryResult, error) {
			return &raceQryResult{Value: 3}, nil
		})
		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryD) (*raceQryResult, error) {
			return &raceQryResult{Value: 4}, nil
		})
		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryE) (*raceQryResult, error) {
			return &raceQryResult{Value: 5}, nil
		})

		close(stop)
		wg.Wait()
	}
}

// TestQueryBus_NewQueryGatewayConcurrent is a regression test for GitHub
// issue #52: NewQueryGateway wrote bus.requestees with no lock at all, so two
// goroutines constructing gateways concurrently (e.g. wiring up multiple
// gateways at startup) performed an unsynchronized concurrent map write.
func TestQueryBus_NewQueryGatewayConcurrent(t *testing.T) {
	for i := 0; i < 50; i++ {
		bus := NewQueryBus()

		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = NewQueryGateway[raceQryA, *raceQryResult](bus)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = NewQueryGateway[raceQryB, *raceQryResult](bus)
		}()

		close(start)
		wg.Wait()
	}
}

// TestQueryBus_ValidateWhileRegistering is a regression test for GitHub issue
// #52: Validate ranged over q.requestees and read q.handlers without taking
// q.mu, while RegisterQueryHandler writes q.handlers under q.mu.
func TestQueryBus_ValidateWhileRegistering(t *testing.T) {
	for i := 0; i < 50; i++ {
		bus := NewQueryBus()
		_ = NewQueryGateway[raceQryA, *raceQryResult](bus)

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
				_ = bus.Validate()
				once.Do(func() { close(running) })
			}
		}()

		<-running

		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryA) (*raceQryResult, error) {
			return &raceQryResult{Value: 1}, nil
		})
		RegisterQueryHandlerFunc(bus, func(ctx context.Context, q raceQryB) (*raceQryResult, error) {
			return &raceQryResult{Value: 2}, nil
		})

		close(stop)
		wg.Wait()
	}
}
