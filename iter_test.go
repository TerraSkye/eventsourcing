package eventsourcing_test

import (
	"context"
	"errors"
	"io"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
)

func TestIteratorBasic(t *testing.T) {
	items := []int{1, 2, 3}
	i := 0

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		if i >= len(items) {
			return 0, io.EOF
		}
		val := items[i]
		i++
		return val, nil
	})

	var got []int

	for iter.Next(t.Context()) {
		got = append(got, iter.Value())
	}

	if iter.Err() != nil {
		t.Fatalf("unexpected error: %v", iter.Err())
	}

	if len(got) != len(items) {
		t.Fatalf("expected %v items, got %v", len(items), len(got))
	}

	for i := range items {
		if got[i] != items[i] {
			t.Errorf("index %d: expected %v got %v", i, items[i], got[i])
		}
	}
}

func TestIteratorEOF(t *testing.T) {
	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		return 0, io.EOF
	})

	ctx := t.Context()

	if iter.Next(ctx) {
		t.Fatal("expected Next() to return false on EOF")
	}

	if iter.Err() != nil {
		t.Fatalf("expected Err() to be nil on EOF, got %v", iter.Err())
	}
}

func TestIteratorError(t *testing.T) {
	expectedErr := errors.New("boom")

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		return 0, expectedErr
	})

	if iter.Next(t.Context()) {
		t.Fatal("expected Next() to return false on error")
	}

	if !errors.Is(iter.Err(), expectedErr) {
		t.Fatalf("expected Err() to be %v, got %v", expectedErr, iter.Err())
	}
}

func TestIteratorAll(t *testing.T) {
	items := []string{"a", "b", "c"}
	i := 0

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (string, error) {
		if i >= len(items) {
			return "", io.EOF
		}
		val := items[i]
		i++
		return val, nil
	})

	got, err := iter.All(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != len(items) {
		t.Fatalf("expected %v items, got %v", len(items), len(got))
	}

	for i := range items {
		if got[i] != items[i] {
			t.Errorf("index %d: expected %v got %v", i, items[i], got[i])
		}
	}
}

func TestIteratorStopsAfterErrorOrEOF(t *testing.T) {
	callCount := 0
	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		callCount++
		if callCount == 1 {
			return 1, nil
		}
		return 0, io.EOF
	})

	if !iter.Next(t.Context()) {
		t.Fatal("expected first Next() to return true")
	}
	if iter.Value() != 1 {
		t.Fatalf("expected Value()=1, got %v", iter.Value())
	}

	if iter.Next(t.Context()) {
		t.Fatal("expected second Next() to return false (EOF)")
	}

	// Ensure Next doesn't call nextFunc again
	for i := 0; i < 5; i++ {
		iter.Next(t.Context())
	}

	if callCount != 2 {
		t.Fatalf("expected nextFunc to be called exactly twice, got %v", callCount)
	}
}

func TestIteratorValueZeroBeforeNext(t *testing.T) {
	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		return 10, nil
	})

	// Value before Next should be zero
	if v := iter.Value(); v != 0 {
		t.Fatalf("expected Value() to be zero before Next, got %v", v)
	}
}

func TestIteratorNoItems(t *testing.T) {
	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		return 0, io.EOF
	})

	items, err := iter.All(t.Context())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty slice, got %v", items)
	}
}

// Benchmark: simple integer iteration using Next() only
func BenchmarkIteratorNext(b *testing.B) {
	ctx := b.Context()

	for n := 0; n < b.N; n++ {
		items := []int{1, 2, 3, 4, 5}
		i := 0

		iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
			if i >= len(items) {
				return 0, io.EOF
			}
			v := items[i]
			i++
			return v, nil
		})

		for iter.Next(ctx) {
			_ = iter.Value()
		}
	}
}

// Benchmark: iterator.Next() + Value() each iteration
func BenchmarkIteratorNextValue(b *testing.B) {
	ctx := b.Context()

	for n := 0; n < b.N; n++ {
		items := []int{1, 2, 3, 4, 5}
		i := 0

		iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
			if i >= len(items) {
				return 0, io.EOF
			}
			v := items[i]
			i++
			return v, nil
		})

		for iter.Next(ctx) {
			_ = iter.Value()
		}
	}
}

// Benchmark: using All()
func BenchmarkIteratorAll(b *testing.B) {
	ctx := b.Context()

	for n := 0; n < b.N; n++ {
		items := []int{1, 2, 3, 4, 5}
		i := 0

		iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
			if i >= len(items) {
				return 0, io.EOF
			}
			v := items[i]
			i++
			return v, nil
		})

		_, _ = iter.All(ctx)
	}
}

// Benchmark: iterator with large values (stress Value() copying)
func BenchmarkIteratorLargeStruct(b *testing.B) {
	type big struct {
		Data [1024]byte // 1 KB struct
	}

	ctx := b.Context()

	for n := 0; n < b.N; n++ {
		items := []big{{}, {}, {}, {}}
		i := 0

		iter := cqrs.NewIteratorFunc(func(ctx context.Context) (big, error) {
			if i >= len(items) {
				return big{}, io.EOF
			}
			v := items[i]
			i++
			return v, nil
		})

		for iter.Next(ctx) {
			_ = iter.Value()
		}
	}
}

// Benchmark: iterator that immediately returns EOF (fast path)
func BenchmarkIteratorEOF(b *testing.B) {
	ctx := b.Context()

	iter := cqrs.NewIteratorFunc(func(ctx context.Context) (int, error) {
		return 0, io.EOF
	})

	for n := 0; n < b.N; n++ {
		_ = iter.Next(ctx)
	}
}
