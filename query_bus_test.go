package eventsourcing

import (
	"context"
	"testing"
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
