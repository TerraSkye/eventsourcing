package eventsourcing

import (
	"context"
	"io"
	"strings"
	"testing"
)

// ---- Stubs ----

type mwTestStoreMiddleware struct {
	timesCalled uint
}

func (m *mwTestStoreMiddleware) Middleware(next EventStore) EventStore {
	return &mwWrappedStore{EventStore: next, onSave: func() { m.timesCalled++ }}
}

type mwWrappedStore struct {
	EventStore
	onSave func()
}

func (s *mwWrappedStore) Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error) {
	s.onSave()
	return s.EventStore.Save(ctx, events, revision)
}

type mwMockStore struct{ saveCount int }

func (s *mwMockStore) Save(_ context.Context, _ []Envelope, _ StreamState) (AppendResult, error) {
	s.saveCount++
	return AppendResult{Successful: true}, nil
}

func (s *mwMockStore) LoadStream(_ context.Context, _ string) (*Iterator[*Envelope], error) {
	return mwEmptyIter(), nil
}

func (s *mwMockStore) LoadStreamFrom(_ context.Context, _ string, _ StreamState) (*Iterator[*Envelope], error) {
	return mwEmptyIter(), nil
}

func (s *mwMockStore) LoadFromAll(_ context.Context, _ StreamState) (*Iterator[*Envelope], error) {
	return mwEmptyIter(), nil
}

func (s *mwMockStore) Close() error { return nil }

func mwEmptyIter() *Iterator[*Envelope] {
	return NewIteratorFunc(func(_ context.Context) (*Envelope, error) { return nil, io.EOF })
}

type mwOrderStore struct {
	EventStore
	order *[]string
	label string
}

func (s *mwOrderStore) Save(ctx context.Context, events []Envelope, revision StreamState) (AppendResult, error) {
	*s.order = append(*s.order, s.label)
	return s.EventStore.Save(ctx, events, revision)
}

// ---- Tests ----

func TestApplyEventStoreMiddleware(t *testing.T) {
	t.Run("no middleware: store works unchanged", func(t *testing.T) {
		inner := &mwMockStore{}
		store := ApplyEventStoreMiddleware(inner)
		store.Save(context.Background(), nil, Any{})
		if inner.saveCount != 1 {
			t.Fatalf("expected 1 save, got %d", inner.saveCount)
		}
	})

	t.Run("one middleware intercepts Save", func(t *testing.T) {
		inner := &mwMockStore{}
		mw := &mwTestStoreMiddleware{}
		store := ApplyEventStoreMiddleware(inner, mw.Middleware)
		store.Save(context.Background(), nil, Any{})
		if mw.timesCalled != 1 {
			t.Fatalf("middleware: expected 1 call, got %d", mw.timesCalled)
		}
		if inner.saveCount != 1 {
			t.Fatalf("inner store: expected 1 save, got %d", inner.saveCount)
		}
	})
}

func TestApplyEventStoreMiddlewareExecution(t *testing.T) {
	inner := &mwMockStore{}
	var out []string

	store := ApplyEventStoreMiddleware(inner,
		EventStoreMiddleware(func(next EventStore) EventStore {
			return &mwWrappedStore{EventStore: next, onSave: func() { out = append(out, "middleware") }}
		}),
	)

	store.Save(context.Background(), nil, Any{})
	store.Save(context.Background(), nil, Any{})

	if strings.Join(out, ",") != "middleware,middleware" {
		t.Fatalf("unexpected output: %v", out)
	}
	if inner.saveCount != 2 {
		t.Fatalf("expected 2 inner saves, got %d", inner.saveCount)
	}
}

func TestApplyEventStoreMiddlewareOrder(t *testing.T) {
	inner := &mwMockStore{}
	var order []string

	store := ApplyEventStoreMiddleware(inner,
		EventStoreMiddleware(func(next EventStore) EventStore {
			return &mwOrderStore{EventStore: next, order: &order, label: "first"}
		}),
		EventStoreMiddleware(func(next EventStore) EventStore {
			return &mwOrderStore{EventStore: next, order: &order, label: "second"}
		}),
	)

	store.Save(context.Background(), nil, Any{})

	want := []string{"first", "second"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, order)
	}
}

func TestApplyEventStoreMiddlewareFuncAndStruct(t *testing.T) {
	inner := &mwMockStore{}
	structMw := &mwTestStoreMiddleware{}
	funcCalls := 0

	store := ApplyEventStoreMiddleware(inner,
		structMw.Middleware,
		EventStoreMiddleware(func(next EventStore) EventStore {
			return &mwWrappedStore{EventStore: next, onSave: func() { funcCalls++ }}
		}),
	)

	store.Save(context.Background(), nil, Any{})

	if structMw.timesCalled != 1 {
		t.Fatalf("struct middleware: expected 1 call, got %d", structMw.timesCalled)
	}
	if funcCalls != 1 {
		t.Fatalf("func middleware: expected 1 call, got %d", funcCalls)
	}
}
