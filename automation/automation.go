package automation

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// errNotImplemented marks the parts of this package that are still a
// design draft: signatures and documentation only.
var errNotImplemented = errors.New("automation: not implemented yet")

// ClaimFunc reads up to max pending items from a todo list and leases them
// for lease, so that other instances of the automation skip them until the
// lease expires.
//
// A claim must be atomic with respect to other instances: two instances
// claiming at the same time must never both get the same item. In SQL,
// FOR UPDATE SKIP LOCKED does this. An item whose lease expired without
// its work succeeding is pending again and will be claimed again.
//
// ClaimFunc returns an empty slice, not an error, when nothing is pending.
type ClaimFunc[T any] func(ctx context.Context, max int, lease time.Duration) ([]T, error)

// WorkFunc does the work for one claimed item, typically ending in a
// command whose event removes the item from the todo list.
//
// Returning an error leaves the item where it is; it is claimed again once
// its lease expires. Since work can be repeated, it must be idempotent; see
// the package documentation.
type WorkFunc[T any] func(ctx context.Context, item T) error

// State is what an automation is doing.
type State int

const (
	// Waiting means a dependency is not live, so the automation does not
	// claim work. [Status.WaitingFor] names the dependency.
	Waiting State = iota

	// Idle means the dependencies are live and the last claim found
	// nothing to do.
	Idle

	// Working means the automation is processing claimed items.
	Working
)

// String returns the state's name in lower case.
func (s State) String() string {
	switch s {
	case Waiting:
		return "waiting"
	case Idle:
		return "idle"
	case Working:
		return "working"
	default:
		return "unknown"
	}
}

// Status describes what an automation is doing, as reported by
// [Automation.Status].
type Status struct {
	// State is what the automation is doing.
	State State

	// WaitingFor is the name of the dependency the automation is waiting
	// for while [Waiting], or "" otherwise.
	WaitingFor string

	// Processed counts the items whose work succeeded since the automation
	// started.
	Processed uint64

	// Failed counts the work attempts that returned an error since the
	// automation started.
	Failed uint64

	// LastError is the error of the most recent failed claim or work
	// attempt, or "" if there was none.
	LastError string
}

// Automation processes the items of a todo list: it claims pending items,
// runs the work for each, and repeats. Create one with [New] and start it
// with [Automation.Run].
//
// Automation implements [projection.Nudger].
type Automation[T any] struct {
	name    string
	waitFor []projection.Dependency
	claim   ClaimFunc[T]
	work    WorkFunc[T]
	cfg     config
	nudge   chan struct{}
	statusV atomic.Pointer[Status]
}

// New creates an automation called name that works off todoList.
//
// todoList is the projection claim reads from. The automation only claims
// work while it is live; pass a [*projection.Runner] when the projection
// runs in this program, or [projection.Remote] when it runs elsewhere.
//
// claim leases pending items and work processes one of them. Without
// options, the automation claims up to 10 items at a time, leases them for
// one minute, and checks for work every five seconds and whenever it is
// nudged.
//
// name identifies the automation in logs, metrics and as the causation of
// the commands it sends. New panics if name is empty or todoList, claim or
// work is nil.
func New[T any](name string, todoList projection.Dependency, claim ClaimFunc[T], work WorkFunc[T], opts ...Option) *Automation[T] {
	switch {
	case name == "":
		panic("automation: New requires a name")
	case todoList == nil:
		panic(fmt.Sprintf("automation %s: New requires a todo list", name))
	case claim == nil || work == nil:
		panic(fmt.Sprintf("automation %s: New requires claim and work functions", name))
	}

	cfg := defaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	a := &Automation[T]{
		name:    name,
		waitFor: append([]projection.Dependency{todoList}, cfg.waitFor...),
		claim:   claim,
		work:    work,
		cfg:     cfg,
		nudge:   make(chan struct{}, 1),
	}
	a.statusV.Store(&Status{State: Waiting, WaitingFor: todoList.Name()})
	return a
}

// Name returns the automation's name, as passed to [New].
func (a *Automation[T]) Name() string { return a.name }

// Run claims and processes work until ctx is done, and then returns
// ctx.Err().
//
// Each round, Run checks that every dependency is live, claims up to the
// batch size of items, and runs the work for each of them in turn. If a
// full batch was claimed, the next round starts right away; otherwise Run
// waits for the interval or a nudge. A failing claim or work function is
// recorded in [Status.LastError] and never stops Run.
//
// When a dependency implements [projection.Watcher], Run registers the
// automation's Nudge with it, so the automation wakes up when the todo list
// changes.
func (a *Automation[T]) Run(ctx context.Context) error {
	panic(errNotImplemented)
}

// Nudge asks the automation to check for work now instead of at its next
// interval. It never blocks, and nudges coalesce. A nudge while a
// dependency is not live has no effect beyond checking that again.
func (a *Automation[T]) Nudge() {
	select {
	case a.nudge <- struct{}{}:
	default: // a nudge is already pending
	}
}

// Status returns what the automation is doing.
func (a *Automation[T]) Status() Status {
	return *a.statusV.Load()
}

var _ projection.Nudger = (*Automation[struct{}])(nil)
