// Package automationtest provides [ClaimAcceptanceTest], the contract a
// todo list's [automation.ClaimFunc] is checked against. Run it from the
// todo list's own tests:
//
//	func TestClaimAcceptance(t *testing.T) {
//		automationtest.ClaimAcceptanceTest(t, func(t *testing.T) automationtest.TodoList {
//			return newOrdersToShipForTest(t)
//		})
//	}
package automationtest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"
)

const (
	// longLease keeps a claim valid for the whole subtest.
	longLease = time.Hour

	// shortLease lets a subtest wait a lease out. Todo lists may store
	// lease ends at a coarse resolution, so expiry is checked after twice
	// this long.
	shortLease = 300 * time.Millisecond
)

// TodoList is a todo list as the suite drives it, with items identified by
// string IDs. An adapter around the real todo list implements it: Add and
// Complete stand in for the projection writing the read model, Claim is the
// [automation.ClaimFunc] under test, returning the claimed items' IDs.
type TodoList interface {
	// Add puts a pending item with the given ID on the list.
	Add(ctx context.Context, id string) error

	// Complete removes the item, as the event that records its work does.
	Complete(ctx context.Context, id string) error

	// Claim leases up to max pending items for lease and returns their
	// IDs.
	Claim(ctx context.Context, max int, lease time.Duration) ([]string, error)
}

// ClaimAcceptanceTest runs the [automation.ClaimFunc] contract against the
// todo lists newList returns, one empty list per subtest. A few subtests
// wait for a lease to expire, so the suite takes a few seconds.
func ClaimAcceptanceTest(t *testing.T, newList func(t *testing.T) TodoList) {
	t.Helper()
	tests := map[string]func(*testing.T, TodoList){
		"EmptyListClaimsNothing":    claimEmpty,
		"ClaimsUpToMax":             claimUpToMax,
		"ClaimedItemsAreSkipped":    claimSkipsLeased,
		"ExpiredLeaseIsReclaimed":   claimExpired,
		"CompletedItemsAreGone":     claimSkipsCompleted,
		"ConcurrentClaimsDisjoint":  claimConcurrentDisjoint,
		"EveryItemIsClaimedOnce":    claimEveryItemOnce,
		"CompletedWhileLeasedIsOff": claimCompletedWhileLeased,
	}
	names := make([]string, 0, len(tests))
	for n := range tests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t.Run(n, func(t *testing.T) { tests[n](t, newList(t)) })
	}
}

func claimEmpty(t *testing.T, l TodoList) {
	got := claim(t, l, 10, longLease)
	if got == nil {
		t.Fatal("Claim on an empty list returned nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("Claim on an empty list = %v, want nothing", got)
	}
}

func claimUpToMax(t *testing.T, l TodoList) {
	add(t, l, "a", "b", "c")
	if got := claim(t, l, 2, longLease); len(got) != 2 {
		t.Fatalf("Claim(max 2) of three items = %v, want two", got)
	}
}

func claimSkipsLeased(t *testing.T, l TodoList) {
	add(t, l, "a", "b")
	first := claim(t, l, 1, longLease)
	second := claim(t, l, 10, longLease)
	if len(first) != 1 || len(second) != 1 || first[0] == second[0] {
		t.Fatalf("claims = %v then %v, want each item once", first, second)
	}
	if third := claim(t, l, 10, longLease); len(third) != 0 {
		t.Fatalf("third claim = %v, want nothing while both items are leased", third)
	}
}

func claimExpired(t *testing.T, l TodoList) {
	add(t, l, "a")
	if got := claim(t, l, 10, shortLease); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("first claim = %v, want [a]", got)
	}
	time.Sleep(2 * shortLease)
	if got := claim(t, l, 10, longLease); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("claim after the lease expired = %v, want [a] again", got)
	}
}

func claimSkipsCompleted(t *testing.T, l TodoList) {
	add(t, l, "a", "b")
	if err := l.Complete(t.Context(), "a"); err != nil {
		t.Fatalf("Complete(a): %v", err)
	}
	if got := claim(t, l, 10, longLease); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("claim after completing a = %v, want [b]", got)
	}
}

func claimCompletedWhileLeased(t *testing.T, l TodoList) {
	add(t, l, "a")
	claim(t, l, 10, shortLease)
	if err := l.Complete(t.Context(), "a"); err != nil {
		t.Fatalf("Complete(a): %v", err)
	}
	time.Sleep(2 * shortLease)
	if got := claim(t, l, 10, longLease); len(got) != 0 {
		t.Fatalf("claim after a leased item was completed = %v, want nothing", got)
	}
}

// claimConcurrentDisjoint claims from several goroutines at once and checks
// no item went to two of them.
func claimConcurrentDisjoint(t *testing.T, l TodoList) {
	const items, claimers = 40, 8
	ids := make([]string, items)
	for i := range ids {
		ids[i] = fmt.Sprintf("item-%02d", i)
	}
	add(t, l, ids...)

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
		errs = make([]error, claimers)
	)
	wg.Add(claimers)
	for c := range claimers {
		go func() {
			defer wg.Done()
			for {
				got, err := l.Claim(t.Context(), 3, longLease)
				if err != nil {
					errs[c] = err
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, id := range got {
					seen[id]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for c, err := range errs {
		if err != nil {
			t.Fatalf("claimer %d: %v", c, err)
		}
	}
	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("item %s claimed %d times, want once", id, seen[id])
		}
	}
}

func claimEveryItemOnce(t *testing.T, l TodoList) {
	add(t, l, "a", "b", "c", "d", "e")
	var got []string
	for range 10 {
		batch := claim(t, l, 2, longLease)
		if len(batch) == 0 {
			break
		}
		got = append(got, batch...)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a", "b", "c", "d", "e"}) {
		t.Fatalf("claimed %v in batches of 2, want every item once", got)
	}
}

func add(t *testing.T, l TodoList, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := l.Add(t.Context(), id); err != nil {
			t.Fatalf("Add(%s): %v", id, err)
		}
	}
}

func claim(t *testing.T, l TodoList, max int, lease time.Duration) []string {
	t.Helper()
	got, err := l.Claim(t.Context(), max, lease)
	if err != nil {
		t.Fatalf("Claim(%d, %s): %v", max, lease, err)
	}
	return got
}
