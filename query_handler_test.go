package eventsourcing

import (
	"context"
	"errors"
	"testing"
)

// --- Test types ---

type GetTaskQuery struct {
	TaskID string
}

func (q GetTaskQuery) ID() []byte { return []byte(q.TaskID) }

type TaskResult struct {
	Title string
}

// --- Tests ---

func TestNewQueryHandlerFunc(t *testing.T) {
	type ctxKey string

	tests := []struct {
		name      string
		ctx       context.Context
		query     GetTaskQuery
		handler   func(ctx context.Context, q GetTaskQuery) (*TaskResult, error)
		wantTitle string
		wantErr   error
		wantNil   bool
	}{
		{
			name:  "returns result",
			ctx:   context.Background(),
			query: GetTaskQuery{TaskID: "123"},
			handler: func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
				return &TaskResult{Title: "My Task"}, nil
			},
			wantTitle: "My Task",
		},
		{
			name:  "propagates error",
			ctx:   context.Background(),
			query: GetTaskQuery{TaskID: "missing"},
			handler: func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
				return nil, errors.New("not found")
			},
			wantErr: errors.New("not found"),
			wantNil: true,
		},
		{
			name:  "receives context",
			ctx:   context.WithValue(context.Background(), ctxKey("user"), "alice"),
			query: GetTaskQuery{TaskID: "1"},
			handler: func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
				val := ctx.Value(ctxKey("user"))
				return &TaskResult{Title: val.(string)}, nil
			},
			wantTitle: "alice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewQueryHandlerFunc(tt.handler)
			result, err := h.HandleQuery(tt.ctx, tt.query)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tt.wantErr)
				}
				if err.Error() != tt.wantErr.Error() {
					t.Errorf("error = %q, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil result, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}
			if result.Title != tt.wantTitle {
				t.Errorf("Title = %q, want %q", result.Title, tt.wantTitle)
			}
		})
	}
}

func TestWrapQueryHandler_DiscardsResultOnErrorWhenMiddlewarePresent(t *testing.T) {

	wantErr := errors.New("partial failure")

	handler := func(ctx context.Context, q GetTaskQuery) (*TaskResult, error) {
		// A handler that intentionally returns a non-zero result alongside
		// an error, e.g. "here's what I found before the failure".
		return &TaskResult{Title: "partial-data"}, wantErr
	}

	t.Run("no middleware: result is preserved alongside the error", func(t *testing.T) {
		bus := NewQueryBus()
		RegisterQueryHandler(bus, NewQueryHandlerFunc(handler))
		gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

		result, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if result == nil || result.Title != "partial-data" {
			t.Fatalf("result = %#v, want partial data preserved alongside the error", result)
		}
	})

	t.Run("with middleware: result is silently zeroed on error", func(t *testing.T) {
		bus := NewQueryBus()
		bus.Use(func(next QueryGateway[Query, any]) QueryGateway[Query, any] {
			return next // a no-op passthrough middleware, e.g. logging/telemetry
		})
		RegisterQueryHandler(bus, NewQueryHandlerFunc(handler))
		gateway := NewQueryGateway[GetTaskQuery, *TaskResult](bus)

		result, err := gateway(context.Background(), GetTaskQuery{TaskID: "1"})
		if !errors.Is(err, wantErr) {
			t.Fatalf("error = %v, want %v", err, wantErr)
		}
		if result == nil || result.Title != "partial-data" {
			t.Errorf("result = %#v, want partial data preserved alongside the error, same as the no-middleware case", result)
		}
	})
}
