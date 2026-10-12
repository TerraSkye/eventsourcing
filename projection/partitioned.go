package projection

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/terraskye/eventsourcing"
)

// flushInterval bounds how long a partition holds events it has received
// before committing them, when neither a full batch nor the end of a read
// made it commit earlier.
const flushInterval = 100 * time.Millisecond

// partitioned holds a partitioned runner's state on this instance.
type partitioned struct {
	mu       sync.Mutex
	owned    []int
	statuses map[int]Status // per running partition
	watchers []func()
}

func defaultMember() string {
	host, _ := os.Hostname()
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func (r *Runner) runPartitioned(ctx context.Context) error {
	cps := r.cfg.checkpoints.(PartitionedCheckpointStore)
	member := r.cfg.member
	if member == "" {
		member = defaultMember()
	}

	// Create leases and checkpoints. Until that works the runner is stalled,
	// since it cannot know which partitions exist.
	delay := retryMin
	for {
		err := r.cfg.leases.Init(ctx, r.name, r.cfg.partitions)
		if err == nil {
			err = cps.InitPartitions(ctx, r.name, r.cfg.partitions)
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.cfg.logger.Error("projection: initialising partitions failed", "projection", r.name, "err", err)
		r.status.Store(&Status{Phase: Stalled, LastError: err.Error(), Partitions: r.cfg.partitions, Owner: member})
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = min(2*delay, retryMax)
	}
	r.publishLocal()

	log := r.cfg.logger.With("projection", r.name, "member", member)
	r.disp = newDispatcher(r.store, r.cfg, r.nudge, log)
	dctx, stopDispatcher := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { r.disp.run(dctx) })

	mgr := &leaseManager{
		name:       r.name,
		member:     member,
		partitions: r.cfg.partitions,
		store:      r.cfg.leases,
		ttl:        r.cfg.leaseTTL,
		renewEvery: r.cfg.leaseTTL / 3,
		stopWait:   r.cfg.leaseTTL,
		log:        log,
		work: func(ctx context.Context, l Lease) error {
			return r.workPartition(ctx, cps, l, member)
		},
		onChange: func(owned []int) {
			r.part.mu.Lock()
			r.part.owned = owned
			r.part.mu.Unlock()
			r.publishLocal()
		},
	}
	mgr.run(ctx) // returns once every partition stopped and was handed over

	stopDispatcher()
	wg.Wait()
	return ctx.Err()
}

// workPartition processes one partition for as long as this instance holds
// its lease.
func (r *Runner) workPartition(ctx context.Context, cps PartitionedCheckpointStore, l Lease, member string) error {
	committed, err := cps.Acquire(ctx, l)
	if err != nil {
		return err // ErrLeaseLost when a newer owner exists
	}
	if committed.Position == 0 && committed.ReplayUntil == 0 {
		// First run: everything already in the log counts as replayed.
		head, err := r.store.Head(ctx)
		if err != nil {
			return r.partitionFailed(ctx, l, member, committed, err)
		}
		committed.ReplayUntil = head
	}

	st := Status{
		Phase:      CatchingUp,
		Position:   committed.Position,
		Head:       r.disp.head.Load(),
		Replaying:  committed.Position < committed.ReplayUntil,
		Owner:      member,
		Partitions: r.cfg.partitions,
		Partition:  l.Partition,
	}
	if prev, ok := r.partitionStatus(l.Partition); ok {
		st.Skipped = prev.Skipped
	}
	r.setPartitionStatus(ctx, st, true)
	defer r.dropPartitionStatus(l.Partition)

	rt := r.disp.register(l.Partition, committed.Position)
	defer r.disp.unregister(l.Partition, rt)

	var (
		batch     []*eventsourcing.Envelope
		upTo      = committed.Position // highest position received
		live      bool                 // the last read reached the head
		lastWrite = time.Now()
		lastSave  = time.Now()
	)

	flush := func(ctx context.Context) error {
		if upTo <= committed.Position {
			return nil
		}
		next := committed
		next.Position = upTo
		err := cps.CommitPartition(ctx, l, next, func(ctx context.Context) error {
			return r.apply(ctx, l.Partition, batch, committed.ReplayUntil)
		})
		if err != nil {
			return err
		}
		hadEvents := len(batch) > 0
		committed, batch, lastWrite = next, nil, time.Now()

		st.Position = committed.Position
		st.Head = max(r.disp.head.Load(), committed.Position)
		st.Replaying = committed.Position < committed.ReplayUntil
		r.setPartitionStatus(ctx, st, false)
		if hadEvents {
			r.notifyWatchers()
		}
		return nil
	}

	setPhase := func(ctx context.Context, p Phase) {
		if st.Phase == p {
			return
		}
		st.Phase, st.LastError = p, ""
		st.Head = max(r.disp.head.Load(), committed.Position)
		r.setPartitionStatus(ctx, st, true)
		lastSave = time.Now()
		r.notifyWatchers()
	}

	fail := func(err error) error {
		if errors.Is(err, ErrLeaseLost) || ctx.Err() != nil {
			return err
		}
		return r.partitionFailed(ctx, l, member, committed, err)
	}

	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The partition is moving or the instance is stopping: commit
			// what was received so the next owner does not redo it. If the
			// lease is already gone, the fence rejects this, which is fine.
			if len(batch) > 0 {
				fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				if err := flush(fctx); err != nil && !errors.Is(err, ErrLeaseLost) {
					r.cfg.logger.Warn("projection: final commit failed, the next owner redoes it",
						"projection", r.name, "partition", l.Partition, "err", err)
				}
				cancel()
			}
			return nil

		case it := <-rt.ch:
			if it.env != nil {
				batch = append(batch, it.env)
				upTo = it.env.GlobalVersion
				if len(batch) >= r.cfg.read.Limit {
					if err := flush(ctx); err != nil {
						return fail(err)
					}
				}
				continue
			}
			upTo = max(upTo, it.progress)
			live = it.live
			if !live {
				setPhase(ctx, CatchingUp)
				continue
			}
			// At the head: commit events right away so the read model is
			// current, and progress alone only now and then.
			if len(batch) > 0 || time.Since(lastWrite) >= r.cfg.pollInterval {
				if err := flush(ctx); err != nil {
					return fail(err)
				}
			}
			if len(batch) == 0 {
				setPhase(ctx, Live)
			}

		case <-ticker.C:
			if len(batch) > 0 || (upTo > committed.Position && time.Since(lastWrite) >= r.cfg.pollInterval) {
				if err := flush(ctx); err != nil {
					return fail(err)
				}
				if live && len(batch) == 0 {
					setPhase(ctx, Live)
				}
			}
			if r.cfg.heartbeat > 0 && time.Since(lastSave) >= r.cfg.heartbeat {
				st.Head = max(r.disp.head.Load(), committed.Position)
				r.setPartitionStatus(ctx, st, true)
				lastSave = time.Now()
			}
		}
	}
}

// partitionFailed reports the partition as stalled and returns err, so the
// lease manager restarts it after a delay from its last checkpoint.
func (r *Runner) partitionFailed(ctx context.Context, l Lease, member string, cp Checkpoint, err error) error {
	st, _ := r.partitionStatus(l.Partition)
	st.Phase = Stalled
	st.LastError = err.Error()
	st.Position = cp.Position
	st.Owner = member
	st.Partitions = r.cfg.partitions
	st.Partition = l.Partition
	r.setPartitionStatus(context.WithoutCancel(ctx), st, true)
	r.cfg.logger.Error("projection: partition failed, retrying from its checkpoint",
		"projection", r.name, "partition", l.Partition, "position", cp.Position, "err", err)
	r.notifyWatchers()
	return err
}

// handlerError is a failure of the projection's handlers (or its buffer)
// on one event, as opposed to a failure of the store or checkpoint store.
type handlerError struct {
	name      string
	partition int // -1 when unpartitioned
	env       *eventsourcing.Envelope
	err       error
}

func (e *handlerError) Error() string {
	where := ""
	if e.partition >= 0 {
		where = fmt.Sprintf("partition %d: ", e.partition)
	}
	return fmt.Sprintf("projection %s: %sevent %d (%s in %s): %v",
		e.name, where, e.env.GlobalVersion, e.env.Event.EventType(), e.env.StreamID, e.err)
}

func (e *handlerError) Unwrap() error { return e.err }

// apply runs the handlers for one batch inside the unit of work ctx carries.
// partition is -1 for an unpartitioned projection. A handler failure is
// returned as a *handlerError.
func (r *Runner) apply(ctx context.Context, partition int, batch []*eventsourcing.Envelope, replayUntil uint64) error {
	if len(batch) == 0 {
		return nil
	}
	if partition >= 0 {
		ctx = context.WithValue(ctx, partitionCtxKey, partition)
	}
	var buf Flusher
	if r.cfg.newBuffer != nil {
		buf = r.cfg.newBuffer()
		ctx = context.WithValue(ctx, bufferKey, buf)
	}
	for _, env := range batch {
		ectx := eventsourcing.WithEnvelope(ctx, env)
		ectx = context.WithValue(ectx, replayingKey, env.GlobalVersion <= replayUntil)
		err := r.handlers.Handle(ectx, env.Event)
		var skipped *eventsourcing.SkippedEventError
		if err != nil && !errors.As(err, &skipped) {
			return &handlerError{name: r.name, partition: partition, env: env, err: err}
		}
	}
	if buf != nil {
		if err := buf.Flush(ctx); err != nil {
			return &handlerError{name: r.name, partition: partition, env: batch[len(batch)-1], err: fmt.Errorf("flushing buffer: %w", err)}
		}
	}
	return nil
}

func (r *Runner) partitionStatus(p int) (Status, bool) {
	r.part.mu.Lock()
	defer r.part.mu.Unlock()
	s, ok := r.part.statuses[p]
	return s, ok
}

// setPartitionStatus records st for its partition and, when save is set,
// publishes it to the StatusStore.
func (r *Runner) setPartitionStatus(ctx context.Context, st Status, save bool) {
	r.part.mu.Lock()
	r.part.statuses[st.Partition] = st
	r.part.mu.Unlock()
	r.publishLocal()

	if !save {
		return
	}
	if ss, ok := r.cfg.checkpoints.(StatusStore); ok {
		if err := ss.SaveStatus(ctx, r.name, st); err != nil && ctx.Err() == nil {
			r.cfg.logger.Warn("projection: saving status failed", "projection", r.name, "partition", st.Partition, "err", err)
		}
	}
}

func (r *Runner) dropPartitionStatus(p int) {
	r.part.mu.Lock()
	delete(r.part.statuses, p)
	r.part.mu.Unlock()
	r.publishLocal()
}

// publishLocal combines the statuses of the partitions this instance runs
// into the status Runner.Status returns.
func (r *Runner) publishLocal() {
	r.part.mu.Lock()
	defer r.part.mu.Unlock()

	agg := Status{Phase: Standby, Partitions: r.cfg.partitions, Owned: slices.Clone(r.part.owned)}
	first := true
	for _, s := range r.part.statuses {
		agg.Owner = s.Owner
		agg.Skipped += s.Skipped
		agg.Head = max(agg.Head, s.Head)
		agg.Replaying = agg.Replaying || s.Replaying
		if first || s.Position < agg.Position {
			agg.Position = s.Position
		}
		if first || phaseRank(s.Phase) > phaseRank(agg.Phase) {
			agg.Phase = s.Phase
			agg.LastError = s.LastError
		}
		first = false
	}
	r.status.Store(&agg)
}

// phaseRank orders phases from furthest along to furthest behind, for
// combining the statuses of several partitions.
func phaseRank(p Phase) int {
	switch p {
	case Live:
		return 0
	case CatchingUp:
		return 1
	case Standby:
		return 2
	case Starting:
		return 3
	default: // Stalled
		return 4
	}
}

func (r *Runner) notifyWatchers() {
	r.part.mu.Lock()
	ws := slices.Clone(r.part.watchers)
	r.part.mu.Unlock()
	for _, fn := range ws {
		fn()
	}
}

// liveRemote and waitRemote answer Live and WaitUntil for a partitioned
// projection, whose partitions run on several instances, from the statuses
// they publish.
func (r *Runner) liveRemote(ctx context.Context) (bool, error) {
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

func (r *Runner) waitRemote(ctx context.Context, position uint64) error {
	ss, ok := r.cfg.checkpoints.(StatusStore)
	if !ok {
		return fmt.Errorf("projection %s: WaitUntil on a partitioned projection requires a checkpoint store that implements StatusStore", r.name)
	}
	interval := min(r.cfg.pollInterval, 100*time.Millisecond)
	for {
		s, found, err := ss.LoadStatus(ctx, r.name)
		if err == nil && found && s.Position >= position {
			return nil
		}
		if !sleep(ctx, interval) {
			return ctx.Err()
		}
	}
}
