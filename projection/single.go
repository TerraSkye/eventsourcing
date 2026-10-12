package projection

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/terraskye/eventsourcing"
)

// single holds the state an unpartitioned runner shares with the methods
// called from other goroutines.
type single struct {
	mu       sync.Mutex
	active   chan struct{} // non-nil while this instance runs the projection; closed when it stops
	changed  chan struct{} // closed and replaced on every status change
	rebuilds chan rebuildRequest
}

type rebuildRequest struct {
	reply chan error
}

// errLockLost ends an active period whose lock was lost.
var errLockLost = errors.New("projection: lock lost")

// memoryCheckpoints is the default CheckpointStore: a map, so a projection
// starts from scratch on every start. It cannot roll back fn's writes.
type memoryCheckpoints struct {
	mu  sync.Mutex
	cps map[string]Checkpoint
}

func (m *memoryCheckpoints) Load(_ context.Context, name string) (Checkpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cps[name], nil
}

func (m *memoryCheckpoints) Commit(ctx context.Context, name string, cp Checkpoint, fn func(ctx context.Context) error) error {
	if err := fn(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cps == nil {
		m.cps = make(map[string]Checkpoint)
	}
	m.cps[name] = cp
	return nil
}

func (r *Runner) setStatus(ctx context.Context, st Status, save bool) {
	r.status.Store(&st)
	r.one.mu.Lock()
	close(r.one.changed)
	r.one.changed = make(chan struct{})
	r.one.mu.Unlock()

	if !save {
		return
	}
	if ss, ok := r.cfg.checkpoints.(StatusStore); ok {
		if err := ss.SaveStatus(ctx, r.name, st); err != nil && ctx.Err() == nil {
			r.cfg.logger.Warn("projection: saving status failed", "projection", r.name, "err", err)
		}
	}
}

func (r *Runner) changedCh() <-chan struct{} {
	r.one.mu.Lock()
	defer r.one.mu.Unlock()
	return r.one.changed
}

func (r *Runner) activeCh() chan struct{} {
	r.one.mu.Lock()
	defer r.one.mu.Unlock()
	return r.one.active
}

func (r *Runner) setActive(on bool) {
	r.one.mu.Lock()
	defer r.one.mu.Unlock()
	switch {
	case on && r.one.active == nil:
		r.one.active = make(chan struct{})
	case !on && r.one.active != nil:
		close(r.one.active)
		r.one.active = nil
	}
}

func (r *Runner) runSingle(ctx context.Context) error {
	locker, _ := r.cfg.checkpoints.(Locker)
	for {
		var lock Lock
		if locker != nil {
			lock = r.acquireLock(ctx, locker)
			if lock == nil {
				return ctx.Err()
			}
		}

		err := r.runActive(ctx, lock)

		if lock != nil {
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if rerr := lock.Release(rctx); rerr != nil {
				r.cfg.logger.Warn("projection: releasing lock failed", "projection", r.name, "err", rerr)
			}
			cancel()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errLockLost) {
			r.cfg.logger.Warn("projection: lock lost, competing again", "projection", r.name)
		}
	}
}

// acquireLock competes for the projection's lock until it gets it or ctx is
// done, in which case it returns nil.
func (r *Runner) acquireLock(ctx context.Context, locker Locker) Lock {
	delay := retryMin
	for {
		lock, ok, err := locker.TryLock(ctx, r.name)
		if ctx.Err() != nil {
			if lock != nil {
				_ = lock.Release(context.WithoutCancel(ctx))
			}
			return nil
		}
		switch {
		case err != nil:
			st := r.Status()
			st.Phase, st.LastError = Stalled, fmt.Sprintf("projection %s: acquiring lock: %v", r.name, err)
			r.setStatus(ctx, st, false)
			r.cfg.logger.Error("projection: acquiring lock failed", "projection", r.name, "err", err)
			if !sleep(ctx, delay) {
				return nil
			}
			delay = min(2*delay, retryMax)
			continue
		case ok:
			return lock
		}
		delay = retryMin
		if st := r.Status(); st.Phase != Standby {
			st.Phase, st.LastError = Standby, ""
			r.setStatus(ctx, st, false)
		}
		if !sleep(ctx, max(r.cfg.pollInterval, time.Second)) {
			return nil
		}
	}
}

// active is the state of one period in which this instance runs the
// projection.
type active struct {
	r        *Runner
	lost     <-chan struct{}
	notify   <-chan struct{}
	cp       Checkpoint
	loaded   bool
	fresh    bool // next catch-up starts from scratch: first start or rebuild
	catching bool // OnCatchingUp ran and OnLive has not yet
	lastGood Phase
	st       Status
	lastSave time.Time
}

func (r *Runner) runActive(ctx context.Context, lock Lock) error {
	a := &active{r: r, lastGood: Starting}
	if lock != nil {
		a.lost = lock.Lost()
		a.st.Owner = lock.Owner()
	}
	a.st.Phase = Starting
	a.st.Version = r.cfg.version
	a.st.Skipped = r.Status().Skipped
	if n, ok := r.store.(eventsourcing.Notifier); ok {
		nctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if ch, err := n.Notify(nctx); err == nil {
			a.notify = ch
		}
	}

	r.setActive(true)
	defer r.setActive(false)
	r.cfg.retry.Reset()
	r.setStatus(ctx, a.st, true)
	a.lastSave = time.Now()

	for {
		if err := a.interrupted(ctx); err != nil {
			return err
		}
		if err := a.step(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, errLockLost) {
				return err
			}
			if err := a.stall(ctx, err); err != nil {
				return err
			}
			continue
		}
		r.cfg.retry.Reset()
	}
}

func (a *active) interrupted(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-a.lost:
		return errLockLost
	default:
		return nil
	}
}

// step does one read and whatever it calls for. It returns after a read
// that was not at the head, or after waiting at the head.
func (a *active) step(ctx context.Context) error {
	r := a.r
	if !a.loaded {
		if err := a.load(ctx); err != nil {
			return err
		}
	}
	a.serveRebuild(ctx)

	res, err := r.store.ReadAll(ctx, a.cp.Position, r.cfg.read)
	if err != nil {
		return fmt.Errorf("projection %s: reading the log after %d: %w", r.name, a.cp.Position, err)
	}
	if res.AtEnd {
		a.st.Head = max(a.st.Head, res.Next)
	} else if h, err := r.store.Head(ctx); err == nil {
		a.st.Head = max(a.st.Head, h)
	}

	if !a.catching && (!res.AtEnd || (a.fresh && len(res.Events) > 0)) {
		a.st.Position = a.cp.Position
		if r.cfg.onCatchingUp != nil {
			if err := r.cfg.onCatchingUp(ctx, a.st); err != nil {
				return fmt.Errorf("projection %s: OnCatchingUp: %w", r.name, err)
			}
		}
		a.catching = true
		a.setPhase(ctx, CatchingUp)
	}

	if len(res.Events) > 0 || res.Next > a.cp.Position {
		if err := a.commitPage(ctx, res); err != nil {
			return err
		}
	}
	a.fresh = false

	if !res.AtEnd {
		if a.st.Phase == Stalled {
			a.setPhase(ctx, CatchingUp)
		}
		a.heartbeat(ctx)
		return nil
	}
	if a.st.Phase != Live {
		if r.cfg.onLive != nil {
			a.st.Position = a.cp.Position
			if err := r.cfg.onLive(ctx, a.st); err != nil {
				return fmt.Errorf("projection %s: OnLive: %w", r.name, err)
			}
		}
		a.catching = false
		a.setPhase(ctx, Live)
	}
	return a.wait(ctx)
}

// load reads the checkpoint and rebuilds the projection if it was built by
// another projector version.
func (a *active) load(ctx context.Context) error {
	r := a.r
	cp, err := r.cfg.checkpoints.Load(ctx, r.name)
	if err != nil {
		return fmt.Errorf("projection %s: loading checkpoint: %w", r.name, err)
	}
	if cp == (Checkpoint{}) {
		// Never ran: start at the configured version, and count what is
		// already in the log as replayed.
		head, err := r.store.Head(ctx)
		if err != nil {
			return fmt.Errorf("projection %s: reading the head of the log: %w", r.name, err)
		}
		cp = Checkpoint{Version: r.cfg.version, ReplayUntil: head}
		a.fresh = true
	} else if cp.Version != r.cfg.version {
		if r.cfg.reset == nil {
			return fmt.Errorf("projection %s: checkpoint was written by version %d, runner is version %d: %w",
				r.name, cp.Version, r.cfg.version, ErrNoReset)
		}
		r.cfg.logger.Info("projection: version changed, rebuilding", "projection", r.name, "from", cp.Version, "to", r.cfg.version)
		if err := a.rebuild(ctx, &cp); err != nil {
			return err
		}
	}
	a.cp, a.loaded = cp, true
	a.st.Position, a.st.Version = cp.Position, cp.Version
	a.st.Replaying = cp.Position < cp.ReplayUntil
	return nil
}

// rebuild wipes the read model and moves cp back to the start, in one unit
// of work.
func (a *active) rebuild(ctx context.Context, cp *Checkpoint) error {
	r := a.r
	if r.cfg.reset == nil {
		return ErrNoReset
	}
	next := Checkpoint{Version: r.cfg.version, ReplayUntil: max(cp.Position, cp.ReplayUntil)}
	if err := r.cfg.checkpoints.Commit(ctx, r.name, next, r.cfg.reset); err != nil {
		return fmt.Errorf("projection %s: rebuild: %w", r.name, err)
	}
	*cp = next
	a.fresh, a.catching = true, false
	a.st.Position, a.st.Version, a.st.Replaying = 0, next.Version, next.ReplayUntil > 0
	return nil
}

// serveRebuild carries out a pending Rebuild call, if any.
func (a *active) serveRebuild(ctx context.Context) {
	select {
	case req := <-a.r.one.rebuilds:
		a.doRebuild(ctx, req)
	default:
	}
}

func (a *active) doRebuild(ctx context.Context, req rebuildRequest) {
	if !a.loaded {
		if err := a.load(ctx); err != nil {
			req.reply <- err
			return
		}
	}
	err := a.rebuild(ctx, &a.cp)
	if err == nil {
		// Report the move back to the start before Rebuild returns, so a
		// WaitUntil called right after it does not see the old position.
		a.lastGood = Starting
		a.st.Phase = Starting
		a.setPhase(ctx, CatchingUp)
	}
	req.reply <- err
}

// commitPage applies one page and moves the checkpoint to res.Next. When
// the page fails because of a handler, its events are retried one at a
// time, each in its own unit of work, so the checkpoint stops right before
// the failing event, which is then either skipped or reported.
func (a *active) commitPage(ctx context.Context, res eventsourcing.ReadResult) error {
	err := a.commit(ctx, res.Next, res.Events)
	if err == nil {
		return nil
	}
	var he *handlerError
	if !errors.As(err, &he) || len(res.Events) == 0 {
		return err
	}

	for _, env := range res.Events {
		if env.GlobalVersion <= a.cp.Position {
			continue
		}
		err := a.commit(ctx, env.GlobalVersion, []*eventsourcing.Envelope{env})
		if err == nil {
			continue
		}
		if !errors.As(err, &he) {
			return err
		}
		if !a.skips() {
			// Leave the checkpoint right before the failing event, past
			// any filtered events in between.
			if env.GlobalVersion-1 > a.cp.Position {
				if cerr := a.commit(ctx, env.GlobalVersion-1, nil); cerr != nil {
					return cerr
				}
			}
			return err
		}
		if err := a.skip(ctx, env, he); err != nil {
			return err
		}
	}
	if res.Next > a.cp.Position {
		return a.commit(ctx, res.Next, nil)
	}
	return nil
}

func (a *active) commit(ctx context.Context, to uint64, events []*eventsourcing.Envelope) error {
	r := a.r
	next := a.cp
	next.Position, next.Version = to, r.cfg.version
	err := r.cfg.checkpoints.Commit(ctx, r.name, next, func(ctx context.Context) error {
		return r.apply(ctx, -1, events, a.cp.ReplayUntil)
	})
	if err != nil {
		return err
	}
	a.cp = next
	a.st.Position = next.Position
	a.st.Head = max(a.st.Head, next.Position)
	a.st.Replaying = next.Position < next.ReplayUntil
	r.setStatus(ctx, a.st, false)
	if len(events) > 0 {
		r.notifyWatchers()
	}
	return nil
}

func (a *active) skips() bool {
	switch a.r.cfg.errorPolicy {
	case SkipAlways:
		return true
	case SkipWhenLive:
		return a.lastGood == Live
	default:
		return false
	}
}

// skip commits env as skipped: recorded as a dead letter when the
// checkpoint store supports it, logged otherwise.
func (a *active) skip(ctx context.Context, env *eventsourcing.Envelope, he *handlerError) error {
	r := a.r
	next := a.cp
	next.Position, next.Version = env.GlobalVersion, r.cfg.version
	err := r.cfg.checkpoints.Commit(ctx, r.name, next, func(ctx context.Context) error {
		if dls, ok := r.cfg.checkpoints.(DeadLetterStore); ok {
			return dls.SaveDeadLetter(ctx, r.name, env, he.err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.cfg.logger.Warn("projection: skipped failing event", "projection", r.name,
		"position", env.GlobalVersion, "stream", env.StreamID, "type", env.Event.EventType(), "err", he.err)
	a.cp = next
	a.st.Position = next.Position
	a.st.Skipped++
	r.setStatus(ctx, a.st, false)
	return nil
}

func (a *active) setPhase(ctx context.Context, p Phase) {
	if a.st.Phase == p {
		return
	}
	a.st.Phase, a.st.LastError = p, ""
	a.st.Position = a.cp.Position
	if p != Stalled {
		a.lastGood = p
	}
	a.r.setStatus(ctx, a.st, true)
	a.lastSave = time.Now()
	a.r.notifyWatchers()
}

func (a *active) heartbeat(ctx context.Context) {
	if a.r.cfg.heartbeat > 0 && time.Since(a.lastSave) >= a.r.cfg.heartbeat {
		a.r.setStatus(ctx, a.st, true)
		a.lastSave = time.Now()
	}
}

// wait waits at the head of the log for a reason to read again.
func (a *active) wait(ctx context.Context) error {
	r := a.r
	timer := time.NewTimer(r.cfg.pollInterval)
	defer timer.Stop()
	var beat <-chan time.Time
	if r.cfg.heartbeat > 0 {
		t := time.NewTimer(max(r.cfg.heartbeat-time.Since(a.lastSave), 0))
		defer t.Stop()
		beat = t.C
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.lost:
			return errLockLost
		case req := <-r.one.rebuilds:
			a.doRebuild(ctx, req)
			return nil
		case <-r.nudge:
			return nil
		case _, ok := <-a.notify:
			if !ok {
				a.notify = nil
				continue
			}
			return nil
		case <-timer.C:
			return nil
		case <-beat:
			a.heartbeat(ctx)
			beat = nil
		}
	}
}

// stall reports err, waits for the retry strategy's next interval and
// returns nil to retry, or an error to end the active period.
func (a *active) stall(ctx context.Context, err error) error {
	r := a.r
	r.cfg.logger.Error("projection: stalled", "projection", r.name, "position", a.cp.Position, "err", err)
	a.st.Phase, a.st.LastError = Stalled, err.Error()
	a.st.Position = a.cp.Position
	r.setStatus(ctx, a.st, true)
	a.lastSave = time.Now()
	r.notifyWatchers()

	d := r.cfg.retry.NextBackOff()
	if d == backoff.Stop {
		d = maxInterval(r.cfg.retry)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.lost:
		return errLockLost
	case req := <-r.one.rebuilds:
		a.doRebuild(ctx, req)
	case <-timer.C:
	}
	return nil
}

// maxInterval is the longest wait a retry strategy uses, for after it
// returned backoff.Stop.
func maxInterval(b backoff.BackOff) time.Duration {
	if e, ok := b.(*backoff.ExponentialBackOff); ok {
		return e.MaxInterval
	}
	return retryMax
}

func (r *Runner) rebuildSingle(ctx context.Context) error {
	if r.cfg.reset == nil {
		return ErrNoReset
	}
	done := r.activeCh()
	if done == nil {
		return ErrNotActive
	}
	req := rebuildRequest{reply: make(chan error, 1)}
	select {
	case r.one.rebuilds <- req:
	case <-done:
		return ErrNotActive
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-req.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runner) liveSingle(ctx context.Context) (bool, error) {
	if r.activeCh() != nil {
		return r.Status().Phase == Live, nil
	}
	ss, ok := r.cfg.checkpoints.(StatusStore)
	if !ok {
		return false, nil
	}
	s, found, err := ss.LoadStatus(ctx, r.name)
	if err != nil || !found {
		return false, err
	}
	return s.Phase == Live, nil
}

func (r *Runner) waitSingle(ctx context.Context, position uint64) error {
	ss, _ := r.cfg.checkpoints.(StatusStore)
	for {
		changed := r.changedCh()
		var poll <-chan time.Time
		if r.activeCh() != nil || ss == nil {
			if r.Status().Position >= position {
				return nil
			}
		} else {
			s, found, err := ss.LoadStatus(ctx, r.name)
			if err == nil && found && s.Position >= position {
				return nil
			}
			poll = time.After(min(r.cfg.pollInterval, 100*time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-poll:
		}
	}
}
