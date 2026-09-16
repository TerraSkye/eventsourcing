package eventsourcing

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestQueryGateway_HandleQuery(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "task-" + q.TaskID}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	result, err := gateway(context.Background(), GetTaskQuery{TaskID: "42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Title != "task-42" {
		t.Errorf("Title = %q, want %q", result.Title, "task-42")
	}
}

func TestQueryGateway_UnregisteredHandler(t *testing.T) {
	bus := NewQueryBus()
	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

	_, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"})
	if err == nil {
		t.Fatal("expected error for unregistered handler")
	}
	if !errors.Is(err, ErrHandlerNotFound) {
		t.Errorf("error = %v, want %v", err, ErrHandlerNotFound)
	}
}

func TestQueryGateway_MultipleGateways(t *testing.T) {
	bus := NewQueryBus()

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "single:" + q.TaskID}, nil
	}))

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q ListTasksQuery) (*TaskListResult, error) {
		return &TaskListResult{Tasks: []string{"x", "y"}}, nil
	}))

	taskGateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	listGateway := NewQueryGateway[ListTasksQuery, *TaskListResult](bus)

	r1, err := taskGateway(context.Background(), GetTaskQuery{TaskID: "7"})
	if err != nil {
		t.Fatalf("taskGateway: unexpected error: %v", err)
	}
	if r1.Title != "single:7" {
		t.Errorf("taskGateway Title = %q, want %q", r1.Title, "single:7")
	}

	r2, err := listGateway(context.Background(), ListTasksQuery{Owner: "bob"})
	if err != nil {
		t.Fatalf("listGateway: unexpected error: %v", err)
	}
	want := []string{"x", "y"}
	if !reflect.DeepEqual(r2.Tasks, want) {
		t.Errorf("listGateway Tasks = %v, want %v", r2.Tasks, want)
	}
}

func TestQueryGateway_PropagatesHandlerError(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return nil, errors.New("db connection lost")
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	_, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "db connection lost" {
		t.Errorf("error = %q, want %q", err.Error(), "db connection lost")
	}
}

func TestQueryGateway_CancelledContext(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return &TaskResult{Title: "ok"}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	_, err := gateway(ctx, GetTaskQuery{TaskID: "1"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want %v", err, context.Canceled)
	}
}

// TestQueryGateway_CancelReleasesCaller asserts the gateway returns as soon as
// the caller's context is cancelled, without waiting for a handler that is
// still running. Before, the caller was held until the handler returned no
// matter what its context said.
func TestQueryGateway_CancelReleasesCaller(t *testing.T) {
	bus := NewQueryBus()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		<-release // never watches ctx
		return &TaskResult{Title: "too late"}, nil
	}))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

	start := time.Now()
	result, err := gateway(ctx, GetTaskQuery{TaskID: "1"})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want one wrapping context.Canceled", err)
	}
	if result != nil {
		t.Errorf("result = %#v, want the zero value once the caller was released", result)
	}
	if elapsed > time.Second {
		t.Errorf("gateway took %v to return, want release at cancellation", elapsed)
	}
}

// TestQueryGateway_HandlerPanicReachesCaller asserts that moving the handler
// call off the caller's goroutine did not turn a panicking handler into a
// process-wide crash: the panic still surfaces at the call site, where a
// caller's recover can see it.
func TestQueryGateway_HandlerPanicReachesCaller(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		panic("handler exploded")
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

	// A cancellable context is what puts the call on its own goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected the handler's panic to reach the caller")
		}
		if r != "handler exploded" {
			t.Fatalf("recovered %#v, want the handler's own panic value", r)
		}
	}()

	_, _ = gateway(ctx, GetTaskQuery{TaskID: "1"})
}

// TestQueryGateway_PanicOnUncancellableContext asserts the same for the path
// that skips the goroutine entirely, where the panic unwinds directly.
func TestQueryGateway_PanicOnUncancellableContext(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		panic("handler exploded")
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

	defer func() {
		if r := recover(); r != "handler exploded" {
			t.Fatalf("recovered %#v, want the handler's own panic value", r)
		}
	}()

	_, _ = gateway(context.Background(), GetTaskQuery{TaskID: "1"})
}

// TestQueryGateway_AbandonedHandlerDoesNotBlock asserts the result channel is
// buffered: a handler that finishes after its caller has gone must be able to
// deliver and exit rather than leaking a goroutine blocked on the send.
func TestQueryGateway_AbandonedHandlerDoesNotBlock(t *testing.T) {
	bus := NewQueryBus()

	release := make(chan struct{})
	finished := make(chan struct{})

	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		<-release
		defer close(finished)
		return &TaskResult{Title: "too late"}, nil
	}), WithQueryTimeout(20*time.Millisecond))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	if _, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want one wrapping context.DeadlineExceeded", err)
	}

	// Let the abandoned handler run to completion; it must get past its
	// return statement.
	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("the abandoned handler never finished; its send blocked on an unbuffered channel")
	}
}
