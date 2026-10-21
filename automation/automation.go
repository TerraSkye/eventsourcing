package automation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
)

// Default values for [Definition]'s tuning fields.
const (
	DefaultBatchSize = 10
	DefaultLease     = time.Minute
	DefaultInterval  = 5 * time.Second
)

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
// automationtest.ClaimAcceptanceTest checks an implementation against this
// contract.
type ClaimFunc[T any] func(ctx context.Context, max int, lease time.Duration) ([]T, error)

// WorkFunc does the work for one claimed item, typically ending in a
// command whose event removes the item from the todo list.
//
// Returning an error leaves the item where it is; it is claimed again once
// its lease expires. Since work can be repeated, it must be idempotent; see
// the package documentation.
type WorkFunc[T any] func(ctx context.Context, item T) error

// Phase is what an automation is doing.
type Phase int

const (
	// Starting means [Automation.Run] has not checked its dependencies yet.
	Starting Phase = iota

	// Waiting means a dependency is not live, so the automation does not
	// claim work. [Status.WaitingFor] names the dependency.
	Waiting

	// Idle means the dependencies are live and the last claim found
	// nothing to do.
	Idle

	// Working means the automation is processing claimed items.
	Working

	// Stalled means checking a dependency or claiming failed; the
	// automation is backing off before it tries again. [Status.LastError]
	// says why.
	Stalled
)

// String returns the phase's lower-case name.
func (p Phase) String() string {
	switch p {
	case Starting:
		return "starting"
	case Waiting:
		return "waiting"
	case Idle:
		return "idle"
	case Working:
		return "working"
	case Stalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// Status describes what an automation is doing, as reported by
// [Automation.Status]. Its shape follows projection.Status, so one
// dashboard reads both.
type Status struct {
	// Phase is what the automation is doing.
	Phase Phase

	// WaitingFor is the name of the dependency the automation is waiting
	// for while [Waiting], or "" otherwise.
	WaitingFor string

	// Processed counts the items whose work succeeded since the automation
	// started.
	Processed uint64

	// Failed counts the work attempts that returned an error since the
	// automation started.
	Failed uint64

	// LastError is the error of the most recent failed check, claim or
	// work attempt, or "" if there was none.
	LastError string
}

// Definition describes an automation: the todo list it works off, how it
// claims and does the work, and its tuning. The zero value for every
// optional field picks sensible defaults.
//
// An automation is usually defined next to its todo list, by a constructor
// in the same package, and handed to [New] by main:
//
//	func NewShipOrders(list *OrdersToShip, runner *projection.Runner, bus eventsourcing.Dispatcher, carrier Carrier) automation.Definition[ShippingTodo] {
//		return automation.Definition[ShippingTodo]{
//			Name:     "ship-orders",
//			TodoList: runner,
//			Claim:    list.Claim,
//			Work:     ShipOrder(bus, carrier),
//		}
//	}
type Definition[T any] struct {
	// Name identifies the automation in logs and in its [Status].
	Name string

	// TodoList is the projection Claim reads from. The automation only
	// claims work while it is live; pass a *projection.Runner when the
	// projection runs in this program, or [Remote] when it runs elsewhere.
	TodoList Dependency

	// Claim leases pending items; see [ClaimFunc].
	Claim ClaimFunc[T]

	// Work processes one claimed item; see [WorkFunc].
	Work WorkFunc[T]

	// Chain returns the chain an item belongs to: the CorrelationID of the
	// event that put the item on the todo list, and that event's EventID
	// as the causation. Keep both on the todo-list row. The ctx Work gets
	// carries them, so the events its commands record join that chain;
	// see [eventsourcing.Envelope.CorrelationID]. Without Chain, the work
	// on each item starts a new chain.
	Chain func(item T) (correlationID, causationID string)

	// WaitFor lists projections that must be live, besides the todo list,
	// before the automation claims work. Use it when the work reads other
	// read models that must be up to date for it to decide correctly.
	WaitFor []Dependency

	// BatchSize is how many items are claimed at once. Items of a batch
	// are processed one after the other and all of them are leased from
	// the moment of the claim, so BatchSize times the time one item takes
	// must stay well below Lease. Default [DefaultBatchSize].
	BatchSize int

	// Lease is how long claimed items are reserved for this instance. An
	// item whose work did not lead to its removal from the todo list
	// within the lease is claimed again: too short a lease makes slow work
	// run twice concurrently, too long a lease delays the retry after a
	// crash. Default [DefaultLease].
	Lease time.Duration

	// Interval is how long an idle automation waits before checking for
	// work again when nothing nudges it. With a todo list in the same
	// program nudges usually come first; with a [Remote] todo list the
	// interval decides how quickly new work is picked up. Default
	// [DefaultInterval].
	Interval time.Duration

	// Logger receives the automation's transitions: waiting for a
	// dependency, stalled with its error, and failed work. Nothing is
	// logged per successful item or while idle. Default slog.Default().
	Logger *slog.Logger
}

// Automation processes the items of a todo list: it claims pending items,
// runs the work for each, and repeats. Create one with [New] and start it
// with [Automation.Run].
type Automation[T any] struct {
	d       Definition[T]
	waitFor []Dependency
	nudge   chan struct{}

	mu     sync.Mutex
	status Status
}

// New creates the automation d describes.
//
// It panics on wiring mistakes: an empty Name, or a nil TodoList, Claim or
// Work.
func New[T any](d Definition[T]) *Automation[T] {
	switch {
	case d.Name == "":
		panic("automation: Definition.Name is required")
	case d.TodoList == nil:
		panic(fmt.Sprintf("automation %q: TodoList is required", d.Name))
	case d.Claim == nil || d.Work == nil:
		panic(fmt.Sprintf("automation %q: Claim and Work are required", d.Name))
	}
	if d.BatchSize <= 0 {
		d.BatchSize = DefaultBatchSize
	}
	if d.Lease <= 0 {
		d.Lease = DefaultLease
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Automation[T]{
		d:       d,
		waitFor: append([]Dependency{d.TodoList}, d.WaitFor...),
		nudge:   make(chan struct{}, 1),
	}
}

// Name returns the automation's name.
func (a *Automation[T]) Name() string { return a.d.Name }

// Run claims and processes work until ctx is done, and then returns
// ctx.Err().
//
// Each round, Run checks that every dependency is live, claims up to
// BatchSize items, and runs the work for each of them in turn. If a full
// batch was claimed, the next round starts right away; otherwise Run waits
// for the interval or a nudge. A dependency that is not live makes Run
// wait ([Waiting]); a failing check or claim makes it back off from 0.5s up
// to 30s ([Stalled]); a failing work call is counted and logged, and its
// item comes back once its lease expires. Run never gives up.
//
// When a dependency implements [Watcher], Run registers the automation's
// Nudge with it, so the automation wakes up when the todo list changes.
// Call Run once per Automation.
func (a *Automation[T]) Run(ctx context.Context) error {
	for _, dep := range a.waitFor {
		if w, ok := dep.(Watcher); ok {
			w.Watch(a.Nudge)
		}
	}

	retry := newRetryBackOff()
	for ctx.Err() == nil {
		dep, err := a.notLive(ctx)
		switch {
		case err != nil:
			a.stall(ctx, fmt.Errorf("check %s: %w", dep, err), retry)
			continue
		case dep != "":
			a.setWaiting(ctx, dep)
			a.wait(ctx)
			continue
		}

		items, err := a.d.Claim(ctx, a.d.BatchSize, a.d.Lease)
		if err != nil {
			a.stall(ctx, fmt.Errorf("claim: %w", err), retry)
			continue
		}
		retry.Reset()
		if len(items) == 0 {
			a.setPhase(Idle, "")
			a.wait(ctx)
			continue
		}

		a.setPhase(Working, "")
		for _, item := range items {
			if ctx.Err() != nil {
				break
			}
			a.doWork(ctx, item)
		}
		if len(items) < a.d.BatchSize {
			a.setPhase(Idle, "")
			a.wait(ctx)
		}
	}
	return ctx.Err()
}

// notLive returns the name of the first dependency that is not live, or
// "" when all are. err is set when a dependency could not be checked; the
// name is then that dependency's.
func (a *Automation[T]) notLive(ctx context.Context) (string, error) {
	for _, dep := range a.waitFor {
		live, err := dep.IsLive(ctx)
		if err != nil {
			return dep.Name(), err
		}
		if !live {
			return dep.Name(), nil
		}
	}
	return "", nil
}

// doWork runs the work for one item and records the outcome.
func (a *Automation[T]) doWork(ctx context.Context, item T) {
	var correlationID, causationID string
	if a.d.Chain != nil {
		correlationID, causationID = a.d.Chain(item)
	}
	workCtx := eventsourcing.WithCausationID(eventsourcing.WithCorrelationID(ctx, correlationID), causationID)
	err := a.d.Work(workCtx, item)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		a.status.Processed++
		return
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return // stopping, not failing
	}
	a.status.Failed++
	a.status.LastError = err.Error()
	a.d.Logger.LogAttrs(ctx, slog.LevelWarn, "automation work failed",
		slog.String("automation", a.d.Name), slog.String("error", err.Error()))
}

// stall reports err, logs it, and backs off.
func (a *Automation[T]) stall(ctx context.Context, err error, retry *backoff.ExponentialBackOff) {
	a.setPhase(Stalled, err.Error())
	wait := retry.NextBackOff()
	a.d.Logger.LogAttrs(ctx, slog.LevelError, "automation stalled",
		slog.String("automation", a.d.Name), slog.String("error", err.Error()), slog.Duration("retry_in", wait))
	sleepCtx(ctx, wait)
}

// setWaiting records that the automation waits for dep, logging the
// change once.
func (a *Automation[T]) setWaiting(ctx context.Context, dep string) {
	a.mu.Lock()
	changed := a.status.Phase != Waiting || a.status.WaitingFor != dep
	a.status.Phase = Waiting
	a.status.WaitingFor = dep
	a.mu.Unlock()
	if changed {
		a.d.Logger.LogAttrs(ctx, slog.LevelInfo, "automation waiting for dependency",
			slog.String("automation", a.d.Name), slog.String("dependency", dep))
	}
}

// setPhase moves the automation to p. lastErr replaces LastError when it
// is not empty.
func (a *Automation[T]) setPhase(p Phase, lastErr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.status.Phase = p
	a.status.WaitingFor = ""
	if lastErr != "" {
		a.status.LastError = lastErr
	}
}

// wait blocks until a nudge, the interval, or ctx is done.
func (a *Automation[T]) wait(ctx context.Context) {
	t := time.NewTimer(a.d.Interval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-a.nudge:
	case <-t.C:
	}
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
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// sleepCtx waits for d or until ctx is done, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// newRetryBackOff returns the capped exponential delay used while the
// automation is stalled, the same as a stalled projection's: 0.5s growing
// to 30s, jittered, never giving up.
func newRetryBackOff() *backoff.ExponentialBackOff {
	return backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(500*time.Millisecond),
		backoff.WithMultiplier(2),
		backoff.WithMaxInterval(30*time.Second),
		backoff.WithMaxElapsedTime(0),
	)
}
