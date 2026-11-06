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
	DefaultBatchSize    = 10
	DefaultLease        = time.Minute
	DefaultPollInterval = 5 * time.Second
	DefaultConcurrency  = 1
	DefaultMaxSegments  = 32

	// DefaultSegmentTerm is how long a held segment is kept while other
	// segments wait for an owner, before it is given back.
	DefaultSegmentTerm = 30 * time.Second
)

// Phase is what an automation is doing.
type Phase int

const (
	// Starting means [Runner.Run] has not checked its dependencies yet.
	Starting Phase = iota

	// Waiting means a dependency is not live, so the automation does not
	// claim work. [Status.WaitingFor] names the dependency.
	Waiting

	// Idle means the dependencies are live and the last check found
	// nothing to do.
	Idle

	// Working means the automation is processing claimed items.
	Working

	// Stalled means a dependency check or the scheduler failed; the
	// automation backs off and retries. [Status.LastError] has the error.
	Stalled
)

// String returns the phase's name.
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
	}
	return fmt.Sprintf("Phase(%d)", int(p))
}

// Status describes what an automation is doing on this instance, as
// reported by [Runner.Status]. Its shape follows projection.Status, so one
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

	// Deferred counts the work attempts that returned [Deferred] since the
	// automation started. They are not failures.
	Deferred uint64

	// LastError is the error of the most recent failed check, scheduler
	// call or work attempt, or "" if there was none.
	LastError string
}

// Summary is what one [Runner.RunOnce] did.
type Summary struct {
	Processed, Failed, Deferred uint64

	// NextEligible is the earliest time a segment paused by [Deferred]
	// during the run becomes eligible again, or zero when none was
	// paused. A scheduler that runs the automation on a timer can use it
	// to decide whether to come back sooner.
	NextEligible time.Time
}

// Definition describes an automation: the todo list it works off, the
// projection that keeps that list, how it does the work, and its tuning.
// The zero value for every optional field picks sensible defaults.
//
// An automation is usually defined next to its todo list, by a constructor
// in the same package, and handed to [NewRunner] by main:
//
//	func NewShipOrders(list *OrdersToShip, runner *projection.Runner, bus eventsourcing.Dispatcher, carrier Carrier) automation.Definition[ShippingTodo] {
//		return automation.Definition[ShippingTodo]{
//			Name:       "ship-orders",
//			TodoList:   list,
//			Projection: runner,
//			Work:       ShipOrder(bus, carrier),
//		}
//	}
//
// That is the whole default: competing consumers over the list, one item
// at a time per instance. Everything else is opt-in, and sits where it
// belongs: a list that implements [Segmented] gets one owner per segment,
// spread over the instances; Concurrency and OrderingKey shape how items of
// one segment run; a [Deferred] return from Work pauses a segment; a
// [Scheduler] replaces the default scheduling entirely.
type Definition[T any] struct {
	// Name identifies the automation in logs, in its [Status], and in the
	// [Store].
	Name string

	// TodoList is the repository the work comes from; see [TodoList]. If
	// it also implements [Segmented], the runner uses that.
	// Required unless Scheduler is set.
	TodoList TodoList[T]

	// Projection is the projection that keeps TodoList. The automation
	// only works while it is live, so it never acts on a list that is
	// still catching up; pass a *projection.Runner when the projection
	// runs in this program, or a *projection.Probe when it runs elsewhere.
	Projection Dependency

	// Store keeps segment leases and pauses for the default scheduler.
	// nil means [NewLocalStore]: per-process leases, right for tests and
	// for an automation that runs in one process only. When several
	// processes run the automation, pass a store they share, such as
	// sqlite.NewAutomationStore or postgres.NewAutomationStore.
	Store Store

	// Scheduler replaces the default scheduling with another source of
	// keyed work; see [Scheduler]. When set, TodoList and Store stay nil.
	Scheduler Scheduler[T]

	// Work processes one claimed item; see [WorkFunc].
	Work WorkFunc[T]

	// OrderingKey is a promise about order. Items with the same key are
	// processed in claim order and never overtake each other: not when they
	// run in parallel with other keys, and not when one of them fails,
	// because a failed item holds back the rest of its key until its lease
	// expires. Items with different keys may run at once, up to
	// Concurrency. nil means every item is independent, which is right for
	// work with no chain in it. A constant key makes the whole segment
	// strictly ordered. Typical keys: an order, a product, a payment, a
	// shipment: the entity the target system sees a sequence of updates to.
	//
	// Concurrency is throughput; OrderingKey is the guarantee. With
	// Concurrency 1 and no key, items run one at a time but a failed item
	// does not hold back the next one.
	OrderingKey func(item T) string

	// WaitFor lists projections that must be live, besides Projection,
	// before the automation works. Use it when the work reads other read
	// models that must be up to date for it to decide correctly.
	WaitFor []Dependency

	// Concurrency is how many items of one segment run at once (an
	// unsegmented list is one segment). Items with the same OrderingKey
	// never run at once, whatever Concurrency is. Default
	// [DefaultConcurrency].
	Concurrency int

	// MaxSegments caps the segments this instance holds at once. When
	// most segments are idle it is the throughput setting: held segments
	// times Concurrency bounds the items outstanding. Default
	// [DefaultMaxSegments].
	MaxSegments int

	// BatchSize is how many items are claimed per segment at once. All of
	// them are leased from the moment of the claim, so BatchSize times the
	// time one item takes must stay well below Lease; an item whose lease
	// ran out while it waited in the batch is skipped, not run late.
	// Default [DefaultBatchSize].
	BatchSize int

	// Lease is how long claimed items are reserved for this instance, and
	// the deadline Work's context carries. An item whose work did not lead
	// to its removal from the todo list within the lease is claimed again:
	// too short a lease makes slow work run twice, too long a lease delays
	// the retry after a crash. Default [DefaultLease].
	Lease time.Duration

	// PollInterval is how long an idle automation waits before checking
	// for work again when nothing nudges it. With a todo list in the same
	// program nudges usually come first; with a todo list run elsewhere
	// the interval decides how quickly new work is picked up. Default
	// [DefaultPollInterval].
	PollInterval time.Duration

	// Logger receives the automation's transitions: waiting for a
	// dependency, stalled with its error, failed work, lost segments.
	// Nothing is logged per successful item or while idle. Default
	// slog.Default().
	Logger *slog.Logger
}

// Runner processes the items of a todo list: it asks its scheduler for
// work, runs the work for each item, and repeats. Create one with
// [NewRunner] and start it with [Runner.Run], or run it to completion with
// [Runner.RunOnce].
type Runner[T any] struct {
	d       Definition[T]
	sched   Scheduler[T]
	waitFor []Dependency
	nudge   chan struct{}

	mu     sync.Mutex
	status Status
}

// NewRunner creates the runner d describes.
//
// It panics on wiring mistakes: an empty Name; a nil Projection or Work;
// neither TodoList nor Scheduler, or both. It logs once which
// capabilities it found on the list, so a list that was meant to be
// segmented but is not shows up at startup rather than in production.
func NewRunner[T any](d Definition[T]) *Runner[T] {
	switch {
	case d.Name == "":
		panic("automation: Definition.Name is required")
	case d.Projection == nil:
		panic(fmt.Sprintf("automation %q: Projection is required", d.Name))
	case d.Work == nil:
		panic(fmt.Sprintf("automation %q: Work is required", d.Name))
	case d.Scheduler == nil && d.TodoList == nil:
		panic(fmt.Sprintf("automation %q: TodoList or Scheduler is required", d.Name))
	case d.Scheduler != nil && (d.TodoList != nil || d.Store != nil):
		panic(fmt.Sprintf("automation %q: Scheduler excludes TodoList and Store", d.Name))
	}
	if d.BatchSize <= 0 {
		d.BatchSize = DefaultBatchSize
	}
	if d.Lease <= 0 {
		d.Lease = DefaultLease
	}
	if d.PollInterval <= 0 {
		d.PollInterval = DefaultPollInterval
	}
	if d.Concurrency <= 0 {
		d.Concurrency = DefaultConcurrency
	}
	if d.MaxSegments <= 0 {
		d.MaxSegments = DefaultMaxSegments
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	sched := d.Scheduler
	if sched == nil {
		if d.Store == nil {
			d.Store = NewLocalStore()
		}
		cs := newClaimScheduler(d)
		d.Logger.LogAttrs(context.Background(), slog.LevelInfo, "automation configured",
			slog.String("automation", d.Name), slog.String("list", cs.capabilities()),
			slog.Int("concurrency", d.Concurrency), slog.Int("max_segments", d.MaxSegments))
		sched = cs
	}
	return &Runner[T]{
		d:       d,
		sched:   sched,
		waitFor: append([]Dependency{d.Projection}, d.WaitFor...),
		nudge:   make(chan struct{}, 1),
	}
}

// Name returns the automation's name.
func (r *Runner[T]) Name() string { return r.d.Name }

// Run works until ctx is done, and then returns ctx.Err().
//
// Each round, Run checks that every dependency is live and asks the
// scheduler for work. Each segment's items run in their own goroutine,
// Concurrency at a time, one at a time per OrderingKey. If work was found,
// the next round starts right away; otherwise Run waits for the poll
// interval or a nudge. A dependency that is not live makes Run wait
// ([Waiting]); a failing check or scheduler call makes it back off from
// 0.5s up to 30s ([Stalled]); a failing work call is counted and logged,
// and its item comes back once its lease expires. Run never gives up.
//
// When a dependency implements [Watcher], Run registers the runner's
// Nudge with it, so the automation wakes up when the todo list changes.
// Call Run once per Runner; the scheduler is closed when it returns.
func (r *Runner[T]) Run(ctx context.Context) error {
	defer r.closeScheduler(ctx)
	for _, dep := range r.waitFor {
		if w, ok := dep.(Watcher); ok {
			w.Watch(r.Nudge)
		}
	}

	retry := newRetryBackOff()
	for ctx.Err() == nil {
		dep, err := r.notLive(ctx)
		switch {
		case err != nil:
			r.stall(ctx, fmt.Errorf("check %s: %w", dep, err), retry)
			continue
		case dep != "":
			r.setWaiting(ctx, dep)
			r.wait(ctx)
			continue
		}

		batches, err := r.sched.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			r.stall(ctx, fmt.Errorf("schedule: %w", err), retry)
			continue
		}
		retry.Reset()
		if len(batches) == 0 {
			r.setPhase(Idle, "")
			r.wait(ctx)
			continue
		}
		r.setPhase(Working, "")
		r.runBatches(ctx, batches)
	}
	return ctx.Err()
}

// RunOnce works until nothing is eligible, then gives every segment back
// and returns what it did. It is the entry point for an automation that a
// timer or a cron job starts: claim and work whatever is pending, then
// stop. A dependency that is not live makes RunOnce wait for it; a failing
// check or scheduler call ends the run with that error; a failing work
// call is counted and its item comes back once its lease expires.
func (r *Runner[T]) RunOnce(ctx context.Context) (Summary, error) {
	defer r.closeScheduler(ctx)
	var sum Summary
	for {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		dep, err := r.notLive(ctx)
		switch {
		case err != nil:
			r.setPhase(Stalled, err.Error())
			return sum, fmt.Errorf("check %s: %w", dep, err)
		case dep != "":
			r.setWaiting(ctx, dep)
			r.wait(ctx)
			continue
		}

		batches, err := r.sched.Next(ctx)
		if err != nil {
			r.setPhase(Stalled, err.Error())
			return sum, fmt.Errorf("schedule: %w", err)
		}
		if len(batches) == 0 {
			r.setPhase(Idle, "")
			return sum, nil
		}
		r.setPhase(Working, "")
		sum.add(r.runBatches(ctx, batches))
	}
}

func (s *Summary) add(o Summary) {
	s.Processed += o.Processed
	s.Failed += o.Failed
	s.Deferred += o.Deferred
	if !o.NextEligible.IsZero() && (s.NextEligible.IsZero() || o.NextEligible.Before(s.NextEligible)) {
		s.NextEligible = o.NextEligible
	}
}

// closeScheduler gives the scheduler a short, uncancelled context to
// release what it holds.
func (r *Runner[T]) closeScheduler(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := r.sched.Close(cctx); err != nil {
		r.d.Logger.LogAttrs(cctx, slog.LevelWarn, "automation scheduler close failed",
			slog.String("automation", r.d.Name), slog.String("error", err.Error()))
	}
}

// notLive returns the name of the first dependency that is not live, or
// "" when all are. err is set when a dependency could not be checked; the
// name is then that dependency's.
func (r *Runner[T]) notLive(ctx context.Context) (string, error) {
	for _, dep := range r.waitFor {
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

// runBatches runs every batch in its own goroutine and waits for all of
// them. Segments are independent, so they run in parallel.
func (r *Runner[T]) runBatches(ctx context.Context, batches []Batch[T]) Summary {
	if len(batches) == 1 {
		return r.runBatch(ctx, batches[0])
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		sum  Summary
		each = make([]Summary, len(batches))
	)
	for i, b := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := r.runBatch(ctx, b)
			mu.Lock()
			each[i] = s
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, s := range each {
		sum.add(s)
	}
	return sum
}

// batchRun is the dispatch state of one segment's batch: a queue per
// ordering key, the keys in flight, and the keys parked by a failure.
type batchRun[T any] struct {
	mu      sync.Mutex
	order   []string // ordering keys in first-appearance order
	queues  map[string][]Item[T]
	inUse   map[string]bool
	parked  map[string]bool
	stopped bool // a deferral: start nothing more
	active  int
	sum     Summary
}

// runBatch runs b's items Concurrency at a time, never two of the same
// ordering key at once, in claim order within a ordering key. A failed item
// parks its ordering key for the rest of the batch, so a later item of the
// same chain cannot overtake it; a deferral stops the batch.
func (r *Runner[T]) runBatch(ctx context.Context, b Batch[T]) Summary {
	run := &batchRun[T]{queues: map[string][]Item[T]{}, inUse: map[string]bool{}, parked: map[string]bool{}}
	for i, it := range b.Items {
		sk := fmt.Sprint(i) // no OrderingKey: every item is its own chain
		if r.d.OrderingKey != nil {
			sk = r.d.OrderingKey(it.Value)
		}
		if _, ok := run.queues[sk]; !ok {
			run.order = append(run.order, sk)
		}
		run.queues[sk] = append(run.queues[sk], it)
	}

	finished := make(chan struct{}, len(b.Items))
	for {
		run.mu.Lock()
		sk, item, ok := run.next(r.d.Concurrency)
		if !ok {
			if run.active == 0 {
				run.mu.Unlock()
				break
			}
			run.mu.Unlock()
			<-finished
			continue
		}
		run.inUse[sk] = true
		run.active++
		run.mu.Unlock()

		go func() {
			out, ok := r.work(ctx, item)
			run.mu.Lock()
			run.inUse[sk] = false
			run.active--
			if !ok {
				run.stopped = true // the run is ending: start nothing more
			} else {
				switch out.Kind {
				case Done:
					run.sum.Processed++
				case Failed:
					run.sum.Failed++
					run.parked[sk] = true
				case DeferredOutcome:
					run.sum.Deferred++
					run.stopped = true
					if run.sum.NextEligible.IsZero() || out.Until.Before(run.sum.NextEligible) {
						run.sum.NextEligible = out.Until
					}
				}
			}
			run.mu.Unlock()
			finished <- struct{}{}
		}()
	}
	return run.sum
}

// next picks the head of the oldest-waiting queue whose ordering key is free
// and not parked, if a slot is free and the batch has not stopped. Called
// with the lock held.
func (b *batchRun[T]) next(concurrency int) (string, Item[T], bool) {
	var zero Item[T]
	if b.stopped || b.active >= concurrency {
		return "", zero, false
	}
	for _, sk := range b.order {
		q := b.queues[sk]
		if len(q) == 0 || b.inUse[sk] || b.parked[sk] {
			continue
		}
		b.queues[sk] = q[1:]
		return sk, q[0], true
	}
	return "", zero, false
}

// work runs Work for one item within its lease, reports the outcome to the
// scheduler and the status, and returns it. ok is false when the run is
// stopping (ctx cancelled): then there is no outcome, nothing is counted,
// and nothing is reported.
func (r *Runner[T]) work(ctx context.Context, item Item[T]) (Outcome, bool) {
	var out Outcome
	if !item.Deadline.IsZero() && !item.Deadline.After(time.Now()) {
		out = Outcome{Kind: Failed, Err: errors.New("lease expired before work started")}
	} else {
		wctx := ctx
		if !item.Deadline.IsZero() {
			var cancel context.CancelFunc
			wctx, cancel = context.WithDeadline(ctx, item.Deadline)
			defer cancel()
		}
		// Every item starts from no chain, so it does not inherit the one
		// Run's ctx carries; Work sets the item's own.
		wctx = eventsourcing.WithCausationID(eventsourcing.WithCorrelationID(wctx, ""), "")

		err := r.d.Work(wctx, item.Value)
		if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return Outcome{}, false // stopping, not failing
		}
		out = classify(err, r.d.Lease)
	}

	if err := r.sched.Done(ctx, item, out); err != nil {
		r.d.Logger.LogAttrs(ctx, slog.LevelWarn, "automation scheduler done failed",
			slog.String("automation", r.d.Name), slog.String("segment", item.Key), slog.String("error", err.Error()))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	switch out.Kind {
	case Done:
		r.status.Processed++
	case DeferredOutcome:
		r.status.Deferred++
	case Failed:
		r.status.Failed++
		r.status.LastError = out.Err.Error()
		r.d.Logger.LogAttrs(ctx, slog.LevelWarn, "automation work failed",
			slog.String("automation", r.d.Name), slog.String("segment", item.Key), slog.String("error", out.Err.Error()))
	}
	return out, true
}

// classify turns Work's return into an outcome: nil is Done, a
// DeferredError is a deferral (a zero Until means lease from now), any
// other error is Failed.
func classify(err error, lease time.Duration) Outcome {
	if err == nil {
		return Outcome{Kind: Done}
	}
	var d *DeferredError
	if errors.As(err, &d) {
		until := d.Until
		if until.IsZero() {
			until = time.Now().Add(lease)
		}
		return Outcome{Kind: DeferredOutcome, Until: until}
	}
	return Outcome{Kind: Failed, Err: err}
}

// stall reports err, logs it, and backs off.
func (r *Runner[T]) stall(ctx context.Context, err error, retry *backoff.ExponentialBackOff) {
	r.setPhase(Stalled, err.Error())
	wait := retry.NextBackOff()
	r.d.Logger.LogAttrs(ctx, slog.LevelError, "automation stalled",
		slog.String("automation", r.d.Name), slog.String("error", err.Error()), slog.Duration("retry_in", wait))
	sleepCtx(ctx, wait)
}

// setWaiting records that the automation waits for dep, logging the
// change once.
func (r *Runner[T]) setWaiting(ctx context.Context, dep string) {
	r.mu.Lock()
	changed := r.status.Phase != Waiting || r.status.WaitingFor != dep
	r.status.Phase = Waiting
	r.status.WaitingFor = dep
	r.mu.Unlock()
	if changed {
		r.d.Logger.LogAttrs(ctx, slog.LevelInfo, "automation waiting for dependency",
			slog.String("automation", r.d.Name), slog.String("dependency", dep))
	}
}

// setPhase moves the automation to p. lastErr replaces LastError when it
// is not empty.
func (r *Runner[T]) setPhase(p Phase, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Phase = p
	r.status.WaitingFor = ""
	if lastErr != "" {
		r.status.LastError = lastErr
	}
}

// wait blocks until a nudge, the poll interval, or ctx is done.
func (r *Runner[T]) wait(ctx context.Context) {
	t := time.NewTimer(r.d.PollInterval)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-r.nudge:
	case <-t.C:
	}
}

// Nudge asks the automation to check for work now instead of at its next
// poll. It never blocks, and nudges coalesce. A nudge while a dependency
// is not live has no effect beyond checking that again.
func (r *Runner[T]) Nudge() {
	select {
	case r.nudge <- struct{}{}:
	default: // a nudge is already pending
	}
}

// Status returns what the automation is doing on this instance.
func (r *Runner[T]) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
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
