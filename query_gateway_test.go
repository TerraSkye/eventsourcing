package eventsourcing

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestQueryGateway_HandleQuery(t *testing.T) {
	bus := NewQueryBus()
	RegisterQueryHandler(bus, NewQueryHandlerFunc(func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		return &TaskResult{Title: "task-" + q.TaskID}, nil
	}))

	gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)
	result, err := gateway.HandleQuery(context.Background(), GetTaskQuery{TaskID: "42"})
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

	_, err := gateway.HandleQuery(context.Background(), GetTaskQuery{TaskID: "1"})
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

	r1, err := taskGateway.HandleQuery(context.Background(), GetTaskQuery{TaskID: "7"})
	if err != nil {
		t.Fatalf("taskGateway: unexpected error: %v", err)
	}
	if r1.Title != "single:7" {
		t.Errorf("taskGateway Title = %q, want %q", r1.Title, "single:7")
	}

	r2, err := listGateway.HandleQuery(context.Background(), ListTasksQuery{Owner: "bob"})
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
	_, err := gateway.HandleQuery(context.Background(), GetTaskQuery{TaskID: "1"})
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
	_, err := gateway.HandleQuery(ctx, GetTaskQuery{TaskID: "1"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want %v", err, context.Canceled)
	}
}
