package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"testing"
)

type nonComparable struct{ tags []string }

func (nonComparable) Error() string { return "non-comparable close failure" }

func counted(s []int, closeErr error, n *int) *Iterator[int] {
	i := 0
	return NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		if i >= len(s) {
			return 0, io.EOF
		}
		v := s[i]
		i++
		return v, nil
	}, func() error { *n++; return closeErr })
}

// drain is the next function most Wrap tests below share: hand through one
// item per call and report the source's end as io.EOF.
//
// io.EOF, not src.Err(): the source's read error and any error from closing
// it both reach the wrapping iterator through Close, so returning them here
// as well would report the same failure twice.
func drain(_ context.Context, src *Iterator[int]) (int, error) {
	if !src.Next() {
		return 0, io.EOF
	}
	return src.Value(), nil
}

func TestFullReadClosesOnce(t *testing.T) {
	n := 0
	it := counted([]int{1, 2, 3}, nil, &n)
	var got []int
	for it.Next() {
		got = append(got, it.Value())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	if n != 1 {
		t.Fatalf("close called %d times, want 1", n)
	}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	if v := it.Value(); v != 0 {
		t.Fatalf("Value after end = %d, want zero", v)
	}
	_ = it.Close()
	if n != 1 {
		t.Fatalf("explicit Close after end called close again: %d", n)
	}
}

func TestEarlyCloseIdempotent(t *testing.T) {
	n := 0
	it := counted([]int{1, 2, 3}, nil, &n)
	it.Next()
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("close called %d times, want 1", n)
	}
	if it.Next() {
		t.Fatal("Next after Close returned true")
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Err after early close = %v, want nil", err)
	}
}

func TestNilIteratorClose(t *testing.T) {
	var it *Iterator[int]
	if err := it.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWrappedEOFEndsCleanly(t *testing.T) {
	it := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, fmt.Errorf("read page: %w", io.EOF)
	}, nil)
	if it.Next() {
		t.Fatal("Next = true")
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Err = %v, want nil", err)
	}
}

func TestUnexpectedEOFIsFailure(t *testing.T) {
	it := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, io.ErrUnexpectedEOF
	}, nil)
	if it.Next() {
		t.Fatal("Next = true")
	}
	if !errors.Is(it.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("Err = %v", it.Err())
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	i := 0
	it := NewIteratorFunc(ctx, func(context.Context) (int, error) { i++; return i, nil },
		func() error { n++; return nil })
	if !it.Next() {
		t.Fatal("first Next = false")
	}
	cancel()
	if it.Next() {
		t.Fatal("Next after cancel = true")
	}
	if !errors.Is(it.Err(), context.Canceled) {
		t.Fatalf("Err = %v", it.Err())
	}
	if n != 1 {
		t.Fatalf("close called %d times", n)
	}
}

func TestCloseErrorAfterCleanEnd(t *testing.T) {
	boom := errors.New("close failed")
	n := 0
	it := counted([]int{1}, boom, &n)
	for it.Next() {
	}
	if !errors.Is(it.Err(), boom) {
		t.Fatalf("Err = %v, want %v", it.Err(), boom)
	}
}

func TestErrJoinsDistinctErrors(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	it := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, readErr
	}, func() error { return closeErr })
	for it.Next() {
	}
	err := it.Err()
	if !errors.Is(err, readErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Err = %v", err)
	}
}

// Wrap, full read: the source close error must surface exactly once.
func TestWrapCloseErrorOnceFullRead(t *testing.T) {
	boom := errors.New("close failed")
	n := 0
	inner := counted([]int{1, 2}, boom, &n)
	out := Wrap(inner, drain, nil)
	var got []int
	for out.Next() {
		got = append(got, out.Value())
	}
	err := out.Err()
	if err == nil {
		t.Fatal("Err = nil")
	}
	if s := err.Error(); s != boom.Error() {
		t.Fatalf("Err = %q, want the close error reported once (%q)", s, boom.Error())
	}
	if n != 1 {
		t.Fatalf("inner close called %d times, want 1", n)
	}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

// Wrap, early close.
func TestWrapCloseErrorOnceEarlyClose(t *testing.T) {
	boom := errors.New("close failed")
	n := 0
	inner := counted([]int{1, 2, 3}, boom, &n)
	out := Wrap(inner, drain, nil)
	out.Next()
	err := out.Close()
	if !errors.Is(err, boom) {
		t.Fatalf("Close = %v", err)
	}
	if s := out.Err().Error(); s != boom.Error() {
		t.Fatalf("Err = %q, want %q", s, boom.Error())
	}
	if n != 1 {
		t.Fatalf("inner close called %d times, want 1", n)
	}
}

// The dedup in Err relies on errors.Is, which cannot match a non-comparable
// error against itself.
func TestWrapNonComparableCloseError(t *testing.T) {
	boom := nonComparable{tags: []string{"a"}}
	n := 0
	inner := counted([]int{1}, boom, &n)
	out := Wrap(inner, drain, nil)
	for out.Next() {
	}
	err := out.Err()
	if err == nil {
		t.Fatal("Err = nil")
	}
	if s := err.Error(); s != boom.Error() {
		t.Fatalf("Err = %q, want the close error reported once (%q)", s, boom.Error())
	}
}

func TestValuesBreakCloses(t *testing.T) {
	n := 0
	it := counted([]int{1, 2, 3}, nil, &n)
	for v := range it.Values() {
		if v == 2 {
			break
		}
	}
	if n != 1 {
		t.Fatalf("close called %d times, want 1", n)
	}
}

func TestValuesPanicCloses(t *testing.T) {
	n := 0
	it := counted([]int{1, 2, 3}, nil, &n)
	func() {
		defer func() { recover() }()
		for range it.Values() {
			panic("boom")
		}
	}()
	if n != 1 {
		t.Fatalf("close called %d times, want 1", n)
	}
}

func TestValuesReportsCloseErrorThroughErr(t *testing.T) {
	boom := errors.New("close failed")
	n := 0
	it := counted([]int{1}, boom, &n)
	var got []int
	for v := range it.Values() {
		got = append(got, v)
	}
	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
	if !errors.Is(it.Err(), boom) {
		t.Fatalf("Err = %v", it.Err())
	}
}

func TestAllOnPartlyConsumed(t *testing.T) {
	n := 0
	it := counted([]int{1, 2, 3}, nil, &n)
	it.Next()
	items, err := it.All()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0] != 2 {
		t.Fatalf("items = %v", items)
	}
}

// All must report a close error that only the deferred Close discovers.
func TestAllReportsCloseError(t *testing.T) {
	boom := errors.New("close failed")
	n := 0
	it := counted([]int{1, 2}, boom, &n)
	_, err := it.All()
	if !errors.Is(err, boom) {
		t.Fatalf("All err = %v, want %v", err, boom)
	}
}

// A nil context must panic at construction, not later: the context cannot
// be supplied after the fact, so an Iterator without one can never run.
func TestNilContext(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewIteratorFunc(nil, ...) did not panic")
		}
		t.Logf("panic: %v", r)
	}()
	NewIteratorFunc(nil, func(context.Context) (int, error) { return 1, nil }, nil)
}

func TestPanickingCloseNotRetried(t *testing.T) {
	n := 0
	it := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, io.EOF
	}, func() error { n++; panic("close panic") })
	func() {
		defer func() { recover() }()
		it.Next()
	}()
	func() {
		defer func() { recover() }()
		_ = it.Close()
	}()
	if n != 1 {
		t.Fatalf("close called %d times, want 1", n)
	}
	t.Logf("Err after panicking close = %v", it.Err())
}

// Inner read error and inner close error must each appear once.
func TestWrapReadAndCloseErrors(t *testing.T) {
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")
	inner := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, readErr
	}, func() error { return closeErr })
	out := Wrap(inner, drain, nil)
	for out.Next() {
	}
	err := out.Err()
	if !errors.Is(err, readErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Err = %v", err)
	}
	if got, want := err.Error(), readErr.Error()+"\n"+closeErr.Error(); got != want {
		t.Fatalf("Err = %q, want %q", got, want)
	}
}

func TestWrapEarlyCloseSurfacesCloseError(t *testing.T) {
	closeErr := errors.New("close failed")
	n := 0
	inner := counted([]int{1, 2, 3}, closeErr, &n)
	out := Wrap(inner, drain, nil)
	out.Next()
	if err := out.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Close = %v", err)
	}
	if n != 1 {
		t.Fatalf("inner close called %d times", n)
	}
}

func TestUninitializedIteratorPanics(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("no panic")
		}
		t.Logf("panic: %v", r)
	}()
	var it Iterator[int]
	it.Next()
}

func TestValuesClosesAndReportsErrOutOfBand(t *testing.T) {
	boom := errors.New("read failed")
	n := 0
	i := 0
	it := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		i++
		if i > 2 {
			return 0, boom
		}
		return i, nil
	}, func() error { n++; return nil })
	var got []int
	for v := range it.Values() {
		got = append(got, v)
	}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	if !errors.Is(it.Err(), boom) {
		t.Fatalf("Err = %v", it.Err())
	}
	if n != 1 {
		t.Fatalf("close called %d times", n)
	}
	// break must also close, exactly once
	n2 := 0
	it2 := counted([]int{1, 2, 3}, nil, &n2)
	for range it2.Values() {
		break
	}
	if n2 != 1 {
		t.Fatalf("close called %d times after break", n2)
	}
	if err := it2.Err(); err != nil {
		t.Fatalf("Err after break = %v", err)
	}
}

// --- Wrap's done hook ---

// done reports a clean end as nil, runs exactly once, and runs only after
// src has been closed — the guarantee that lets it end a span or record a
// metric covering the whole iteration.
func TestWrapDoneAfterCleanEnd(t *testing.T) {
	closes, dones := 0, 0
	var doneErr error
	closedBefore := false

	src := counted([]int{1, 2}, nil, &closes)
	out := Wrap(src,
		drain,
		func(err error) { dones++; doneErr = err; closedBefore = closes == 1 })

	got, err := out.All()
	if err != nil {
		t.Fatalf("All = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("All = %v, want 2 items", got)
	}
	if dones != 1 {
		t.Fatalf("done called %d times, want 1", dones)
	}
	if doneErr != nil {
		t.Fatalf("done err = %v, want nil on a clean end", doneErr)
	}
	if !closedBefore {
		t.Fatal("done ran before src was closed")
	}
}

// A source failure reaches done, and reaches the caller through Err exactly
// once even though next never returned it.
func TestWrapDoneSeesSourceError(t *testing.T) {
	boom := errors.New("read failed")
	dones := 0
	var doneErr error

	src := NewIteratorFunc(context.Background(), func(context.Context) (int, error) {
		return 0, boom
	}, nil)
	out := Wrap(src,
		drain,
		func(err error) { dones++; doneErr = err })

	for out.Next() {
		t.Fatal("Next = true on a failing source")
	}
	if dones != 1 {
		t.Fatalf("done called %d times, want 1", dones)
	}
	if !errors.Is(doneErr, boom) {
		t.Fatalf("done err = %v, want %v", doneErr, boom)
	}
	if err := out.Err(); !errors.Is(err, boom) {
		t.Fatalf("Err = %v, want %v", err, boom)
	}
	if got, want := out.Err().Error(), boom.Error(); got != want {
		t.Fatalf("Err = %q, want the error reported once (%q)", got, want)
	}
}

// An early Close still closes src and still runs done, so a caller that
// stops reading halfway is instrumented like any other.
func TestWrapDoneOnEarlyClose(t *testing.T) {
	closes, dones := 0, 0

	src := counted([]int{1, 2, 3}, nil, &closes)
	out := Wrap(src,
		drain,
		func(error) { dones++ })

	if !out.Next() {
		t.Fatal("Next = false")
	}
	if err := out.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if closes != 1 {
		t.Fatalf("src closed %d times, want 1", closes)
	}
	if dones != 1 {
		t.Fatalf("done called %d times, want 1", dones)
	}
}

// next may call src.Next any number of times per item, so a wrapper can
// drop items; done still sees the whole iteration.
func TestWrapFiltersItems(t *testing.T) {
	closes, dones := 0, 0

	src := counted([]int{1, 2, 3, 4, 5}, nil, &closes)
	out := Wrap(src,
		func(_ context.Context, src *Iterator[int]) (int, error) {
			for src.Next() {
				if src.Value()%2 == 0 {
					return src.Value(), nil
				}
			}
			return 0, io.EOF
		},
		func(error) { dones++ })

	got, err := out.All()
	if err != nil {
		t.Fatalf("All = %v", err)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("All = %v, want [2 4]", got)
	}
	if closes != 1 || dones != 1 {
		t.Fatalf("src closed %d times, done called %d times, want 1 and 1", closes, dones)
	}
}

// --- Benchmarks ---

func items(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i
	}
	return s
}

var sink int

// Baseline: what the loop costs with no iterator at all.
func BenchmarkRangeSlice(b *testing.B) {
	for _, n := range []int{10, 1000, 100000} {
		s := items(n)
		b.Run(name(n), func(b *testing.B) {
			for b.Loop() {
				for _, v := range s {
					sink += v
				}
			}
		})
	}
}

func name(n int) string {
	switch n {
	case 10:
		return "n=10"
	case 1000:
		return "n=1000"
	default:
		return "n=100000"
	}
}

// Next with context.Background: Err() is a nil return, no lock.
func BenchmarkNextBackground(b *testing.B) {
	ctx := context.Background()
	for _, n := range []int{10, 1000, 100000} {
		s := items(n)
		b.Run(name(n), func(b *testing.B) {
			for b.Loop() {
				it := NewSliceIterator(ctx, s)
				for it.Next() {
					sink += it.Value()
				}
			}
		})
	}
}

// Next with a cancellable context: Err() takes the cancelCtx mutex.
func BenchmarkNextCancelCtx(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []int{10, 1000, 100000} {
		s := items(n)
		b.Run(name(n), func(b *testing.B) {
			for b.Loop() {
				it := NewSliceIterator(ctx, s)
				for it.Next() {
					sink += it.Value()
				}
			}
		})
	}
}

func BenchmarkValues(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []int{10, 1000, 100000} {
		s := items(n)
		b.Run(name(n), func(b *testing.B) {
			for b.Loop() {
				for v := range NewSliceIterator(ctx, s).Values() {
					sink += v
				}
			}
		})
	}
}

// seq2 adapts an Iterator to the iter.Seq2 shape a Seq2-only API would
// expose. The package no longer provides one; this exists to measure what
// such an API would cost a consumer that needs pull access.
func seq2[T any](it *Iterator[T]) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		defer it.Close()
		for it.Next() {
			if !yield(it.Value(), nil) {
				return
			}
		}
		if err := it.Err(); err != nil {
			var zero T
			yield(zero, err)
		}
	}
}

// The rev-2 claim: pulling from a Seq2 costs a coroutine switch per item.
func BenchmarkPull2OverSeq(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, n := range []int{10, 1000, 100000} {
		s := items(n)
		b.Run(name(n), func(b *testing.B) {
			for b.Loop() {
				next, stop := iter.Pull2(seq2(NewSliceIterator(ctx, s)))
				for {
					v, _, ok := next()
					if !ok {
						break
					}
					sink += v
				}
				stop()
			}
		})
	}
}
