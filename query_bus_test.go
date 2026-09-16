package eventsourcing

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

type ifaceQuery struct{ ID_ string }

func (q ifaceQuery) ID() []byte { return []byte(q.ID_) }

// Two distinct, interface-typed result types for the SAME query type.
type taskView interface{ TaskTitle() string }
type userView interface{ UserName() string }

type taskViewImpl struct{ title string }

func (t taskViewImpl) TaskTitle() string { return t.title }

type userViewImpl struct{ name string }

func (u userViewImpl) UserName() string { return u.name }

// TestQueryBusKey_InterfaceResultCollapsesToNil is a regression test for
// GitHub issue #53: the registry key used to be built with
// fmt.Sprintf("%T|%T", *new(T), *new(R)), and *new(R) for an interface R is a
// nil interface value with no dynamic type, so %T rendered it as the literal
// string "<nil>" regardless of which interface R actually was.
func TestQueryBusKey_InterfaceResultCollapsesToNil(t *testing.T) {
	key1 := queryKey[ifaceQuery, taskView]()
	key2 := queryKey[ifaceQuery, userView]()

	if key1 == key2 {
		t.Fatalf("distinct result types produce the same bus key: %q == %q", key1, key2)
	}
}

// TestQueryBus_SameQueryDifferentInterfaceResults is a regression test for
// GitHub issue #53: registering two handlers for the same query type but
// different interface result types used to collide on one map key, so the
// second registration panicked with ErrDuplicateHandler even though no
// duplicate existed.
func TestQueryBus_SameQueryDifferentInterfaceResults(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (taskView, error) {
		return taskViewImpl{title: "task-" + q.ID_}, nil
	}))

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("registering a second handler for the same query with a "+
					"different interface result type panicked: %v", r)
			}
		}()
		RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (userView, error) {
			return userViewImpl{name: "user-" + q.ID_}, nil
		}))
	}()

	taskGateway := NewQueryGateway[ifaceQuery, taskView](bus)
	userGateway := NewQueryGateway[ifaceQuery, userView](bus)

	got1, err := taskGateway(context.Background(), ifaceQuery{ID_: "1"})
	if err != nil {
		t.Fatalf("taskGateway: unexpected error: %v", err)
	}
	if got1.TaskTitle() != "task-1" {
		t.Errorf("taskGateway = %q, want %q", got1.TaskTitle(), "task-1")
	}

	got2, err := userGateway(context.Background(), ifaceQuery{ID_: "2"})
	if err != nil {
		t.Fatalf("userGateway: unexpected error: %v", err)
	}
	if got2.UserName() != "user-2" {
		t.Errorf("userGateway = %q, want %q", got2.UserName(), "user-2")
	}
}

// TestQueryGateway_InterfaceResultResolvesWrongHandler is a regression test
// for GitHub issue #53, isolating the lookup half of the bug: with only one
// handler registered, for (ifaceQuery, taskView), a gateway built for the
// unregistered pair (ifaceQuery, userView) used to find the taskView handler
// anyway because both interface result types keyed as "<nil>".
func TestQueryGateway_InterfaceResultResolvesWrongHandler(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ifaceQuery) (taskView, error) {
		return taskViewImpl{title: "task-" + q.ID_}, nil
	}))

	userGateway := NewQueryGateway[ifaceQuery, userView](bus)

	_, err := userGateway(context.Background(), ifaceQuery{ID_: "1"})
	if err == nil {
		t.Fatal("expected an error for an unregistered (query, result) pair")
	}
	if !errors.Is(err, ErrHandlerNotFound) {
		t.Errorf("error = %v, want it to wrap ErrHandlerNotFound; "+
			"the gateway matched a foreign handler because both interface "+
			"result types key as <nil>", err)
	}
}

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

// slowQuery is the query type used by the timeout tests.
type slowQuery struct{ ID_ string }

func (q slowQuery) ID() []byte { return []byte(q.ID_) }

// TestWithQueryTimeout_AppliesDeadlineToHandler asserts the per-handler
// default deadline reaches the handler's own context, so a handler that
// honours ctx stops on its own.
func TestWithQueryTimeout_AppliesDeadlineToHandler(t *testing.T) {
	bus := NewQueryBus()

	deadlines := make(chan time.Duration, 1)
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q slowQuery) (*TaskResult, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlines <- 0
			return &TaskResult{Title: "no deadline"}, nil
		}
		deadlines <- time.Until(deadline)
		return &TaskResult{Title: "ok"}, nil
	}), WithQueryTimeout(time.Minute))

	gateway := NewQueryGateway[slowQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), slowQuery{ID_: "1"}); err != nil {
		t.Fatalf("gateway: %v", err)
	}

	remaining := <-deadlines
	if remaining <= 0 {
		t.Fatalf("handler saw no deadline; WithQueryTimeout did not reach its context")
	}
	if remaining > time.Minute {
		t.Errorf("handler deadline is %v away, want at most the minute configured", remaining)
	}
}

// TestWithQueryTimeout_ReleasesCallerFromUncooperativeHandler covers the gap
// this option closes: before it, a handler that never returns held its caller
// forever. The handler here deliberately ignores ctx, which is the case a
// deadline on the context alone cannot solve.
func TestWithQueryTimeout_ReleasesCallerFromUncooperativeHandler(t *testing.T) {
	bus := NewQueryBus()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q slowQuery) (*TaskResult, error) {
		<-release // never watches ctx
		return &TaskResult{Title: "too late"}, nil
	}), WithQueryTimeout(20*time.Millisecond))

	gateway := NewQueryGateway[slowQuery, *TaskResult](bus)

	start := time.Now()
	result, err := gateway(context.Background(), slowQuery{ID_: "1"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if result != nil {
		t.Errorf("result = %#v, want the zero value once the deadline passed", result)
	}
	if elapsed > time.Second {
		t.Errorf("gateway took %v to return, want roughly the 20ms deadline", elapsed)
	}
}

// TestWithQueryTimeout_CallerDeadlineWinsWhenEarlier asserts the registered
// timeout is a ceiling rather than an override.
func TestWithQueryTimeout_CallerDeadlineWinsWhenEarlier(t *testing.T) {
	bus := NewQueryBus()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q slowQuery) (*TaskResult, error) {
		<-release
		return &TaskResult{Title: "too late"}, nil
	}), WithQueryTimeout(time.Hour))

	gateway := NewQueryGateway[slowQuery, *TaskResult](bus)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := gateway(ctx, slowQuery{ID_: "1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want one wrapping context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gateway took %v to return, want the caller's 20ms deadline to win over the registered hour", elapsed)
	}
}

// TestWithQueryTimeout_NotSetLeavesHandlerUnbounded asserts registering
// without the option keeps the previous behaviour: no deadline is imposed, and
// a handler is free to take as long as it likes.
func TestWithQueryTimeout_NotSetLeavesHandlerUnbounded(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q slowQuery) (*TaskResult, error) {
		if _, ok := ctx.Deadline(); ok {
			return nil, errors.New("handler was given a deadline it never asked for")
		}
		time.Sleep(10 * time.Millisecond)
		return &TaskResult{Title: "ok"}, nil
	}))

	gateway := NewQueryGateway[slowQuery, *TaskResult](bus)
	result, err := gateway(context.Background(), slowQuery{ID_: "1"})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	if result.Title != "ok" {
		t.Errorf("Title = %q, want %q", result.Title, "ok")
	}
}

// TestWithQueryTimeout_ZeroIsNoDeadline asserts a non-positive duration is
// treated the same as not passing the option at all.
func TestWithQueryTimeout_ZeroIsNoDeadline(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q slowQuery) (*TaskResult, error) {
		if _, ok := ctx.Deadline(); ok {
			return nil, errors.New("a zero timeout still produced a deadline")
		}
		return &TaskResult{Title: "ok"}, nil
	}), WithQueryTimeout(0))

	gateway := NewQueryGateway[slowQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), slowQuery{ID_: "1"}); err != nil {
		t.Fatalf("gateway: %v", err)
	}
}
