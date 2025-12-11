package eventsourcing

import (
	"context"
	"errors"
	"io"
	"testing"
)

// --- Helper type for struct tests ---
type TestStruct struct {
	ID   string
	Val  int
	Name string
}

// --- Full Iterator[T] test suite ---

func TestIterator_Primitive(t *testing.T) {
	items := []int{1, 2, 3, 4, 5}
	i := 0
	iter := NewIterator(func(ctx context.Context) (int, error) {
		if i >= len(items) {
			return 0, io.EOF
		}
		val := items[i]
		i++
		return val, nil
	})

	var results []int
	for iter.Next(context.Background()) {
		results = append(results, iter.Value())
	}

	if iter.Err() != nil {
		t.Fatalf("unexpected error: %v", iter.Err())
	}
	if len(results) != len(items) {
		t.Fatalf("expected %d items, got %d", len(items), len(results))
	}
	for idx, v := range items {
		if results[idx] != v {
			t.Errorf("expected %d at index %d, got %d", v, idx, results[idx])
		}
	}
}

func TestIterator_StructPointer(t *testing.T) {
	items := []*TestStruct{
		{ID: "a", Val: 1},
		{ID: "b", Val: 2},
		{ID: "c", Val: 3},
	}
	i := 0
	iter := NewIterator(func(ctx context.Context) (*TestStruct, error) {
		if i >= len(items) {
			return nil, io.EOF
		}
		v := items[i]
		i++
		return v, nil
	})

	results, err := iter.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != len(items) {
		t.Fatalf("expected %d items, got %d", len(items), len(results))
	}
	for idx, v := range items {
		if results[idx] != v {
			t.Errorf("expected %+v at index %d, got %+v", v, idx, results[idx])
		}
	}
}

func TestIterator_StructNonPointer(t *testing.T) {
	type MyStruct struct {
		A int
		B string
	}
	items := []MyStruct{
		{1, "x"},
		{2, "y"},
	}
	i := 0
	iter := NewIterator(func(ctx context.Context) (MyStruct, error) {
		if i >= len(items) {
			return MyStruct{}, io.EOF
		}
		val := items[i]
		i++
		return val, nil
	})

	results, err := iter.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != len(items) || results[0].A != 1 || results[1].B != "y" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestIterator_EmptySlice(t *testing.T) {
	iter := NewIterator(func(ctx context.Context) (int, error) {
		return 0, io.EOF
	})

	if iter.Next(context.Background()) {
		t.Fatal("expected Next() to return false for empty iterator")
	}

	results, err := iter.All(context.Background())
	if err != nil {
		t.Fatalf("expected nil error for empty iterator, got %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d", len(results))
	}
}

func TestIterator_ErrorPropagation(t *testing.T) {
	expectedErr := errors.New("something went wrong")
	iter := NewIterator(func(ctx context.Context) (int, error) {
		return 0, expectedErr
	})

	if iter.Next(context.Background()) {
		t.Fatal("expected Next() to return false due to error")
	}
	if iter.Err() != expectedErr {
		t.Fatalf("expected error %v, got %v", expectedErr, iter.Err())
	}
}

func TestIterator_ErrorAfterSomeValues(t *testing.T) {
	i := 0
	iter := NewIterator(func(ctx context.Context) (int, error) {
		i++
		if i <= 2 {
			return i, nil
		}
		return 0, errors.New("iteration failed")
	})

	if !iter.Next(context.Background()) || iter.Value() != 1 {
		t.Fatal("expected first value 1")
	}
	if !iter.Next(context.Background()) || iter.Value() != 2 {
		t.Fatal("expected second value 2")
	}

	if iter.Next(context.Background()) {
		t.Fatal("expected Next() false after error")
	}
	if iter.Err() == nil || iter.Err().Error() != "iteration failed" {
		t.Fatalf("expected error 'iteration failed', got %v", iter.Err())
	}
	if iter.Value() != 2 {
		t.Fatalf("expected Value() to stay at last valid item, got %d", iter.Value())
	}
}

func TestIterator_MultipleNextAfterDone(t *testing.T) {
	iter := NewIterator(func(ctx context.Context) (int, error) {
		return 1, io.EOF
	})

	if iter.Next(context.Background()) {
		t.Fatal("expected Next() to return false for first value")
	}
	if iter.Value() != 0 {
		t.Fatalf("expected 0, got %d", iter.Value())
	}

	if iter.Next(context.Background()) {
		t.Fatal("expected Next() to return false after EOF")
	}
	if iter.Next(context.Background()) {
		t.Fatal("expected Next() to consistently return false")
	}
}

func TestIterator_ValueBeforeNext(t *testing.T) {
	iter := NewIterator(func(ctx context.Context) (int, error) {
		return 0, io.EOF
	})

	if v := iter.Value(); v != 0 {
		t.Fatalf("expected zero value before Next(), got %d", v)
	}
}

func TestIterator_ValueAfterDone(t *testing.T) {
	iter := NewIterator(func(ctx context.Context) (int, error) {
		return 42, nil
	})
	// simulate EOF after one item
	called := false
	iter.nextFunc = func(ctx context.Context) (int, error) {
		if !called {
			called = true
			return 42, nil
		}
		return 0, io.EOF
	}

	if !iter.Next(context.Background()) || iter.Value() != 42 {
		t.Fatal("expected first value 42")
	}
	if iter.Next(context.Background()) {
		t.Fatal("expected Next() false after EOF")
	}
	if iter.Value() != 42 {
		t.Fatalf("expected Value() to still return last valid item, got %d", iter.Value())
	}
}

func TestIterator_AllPartialConsumption(t *testing.T) {
	items := []int{10, 20, 30, 40}
	i := 0
	iter := NewIterator(func(ctx context.Context) (int, error) {
		if i >= len(items) {
			return 0, io.EOF
		}
		val := items[i]
		i++
		return val, nil
	})

	// consume first
	if !iter.Next(context.Background()) || iter.Value() != 10 {
		t.Fatal("expected first value 10")
	}

	results, err := iter.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 || results[0] != 20 || results[2] != 40 {
		t.Fatalf("unexpected remaining results: %+v", results)
	}
}

func TestIterator_ContextPassedThrough(t *testing.T) {
	ctx := context.WithValue(context.Background(), "key", "value")
	calls := 0
	iter := NewIterator(func(ctx context.Context) (int, error) {
		calls++
		v := ctx.Value("key")
		if v != "value" {
			return 0, errors.New("context not passed correctly")
		}
		if calls == 1 {
			return 42, nil
		}
		return 0, io.EOF
	})

	if !iter.Next(ctx) {
		t.Fatal("expected Next() to return true")
	}
	if iter.Value() != 42 {
		t.Fatalf("expected 42, got %d", iter.Value())
	}
	if iter.Next(ctx) {
		t.Fatal("expected Next() to return false after EOF")
	}
	if iter.Err() != nil {
		t.Fatalf("unexpected error: %v", iter.Err())
	}
}

func TestIterator_PanicInsideNextFunc(t *testing.T) {
	iter := NewIterator(func(ctx context.Context) (int, error) {
		panic("oops")
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic to propagate")
		}
	}()

	iter.Next(context.Background())
}
