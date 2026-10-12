package projection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// leaseManager keeps this instance's fair share of a partitioned
// projection's partitions: it heartbeats, renews its leases, releases
// partitions when other instances join, claims free or expired ones, and
// runs work for every partition it holds.
//
// All fields except slots are only touched by the goroutine running run.
type leaseManager struct {
	name       string
	member     string
	partitions int
	store      LeaseStore
	ttl        time.Duration
	renewEvery time.Duration
	stopWait   time.Duration
	work       func(ctx context.Context, l Lease) error
	log        *slog.Logger
	onChange   func(owned []int)

	mu    sync.Mutex // guards slots for owned(); mutated only by run
	slots map[int]*slot

	lost      chan *slot
	lastRenew time.Time
}

type slot struct {
	lease  Lease
	cancel context.CancelFunc
	done   chan struct{}
}

// run manages leases until ctx is done. It then stops every worker,
// releases their leases and leaves, so other instances take over at once.
func (m *leaseManager) run(ctx context.Context) {
	m.slots = make(map[int]*slot)
	m.lost = make(chan *slot, m.partitions)
	m.lastRenew = time.Now()

	ticker := time.NewTicker(m.renewEvery)
	defer ticker.Stop()

	m.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case s := <-m.lost:
			m.forget(s)
		case <-ticker.C:
			m.tick(ctx)
		}
	}
}

func (m *leaseManager) owned() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	ps := make([]int, 0, len(m.slots))
	for p := range m.slots {
		ps = append(ps, p)
	}
	sort.Ints(ps)
	return ps
}

func (m *leaseManager) tick(ctx context.Context) {
	live, err := m.store.Heartbeat(ctx, m.name, m.member, m.ttl)
	if err != nil {
		m.log.Error("projection: heartbeat failed", "err", err)
		m.checkRenewDeadline()
		return
	}
	target := (m.partitions + max(live, 1) - 1) / max(live, 1)

	held, err := m.store.Renew(ctx, m.name, m.member, m.ttl)
	if err != nil {
		m.log.Error("projection: renewing leases failed", "err", err)
		m.checkRenewDeadline()
		return
	}
	m.lastRenew = time.Now()

	// Anything we run that the store no longer lists under our fence was
	// taken over.
	var lost []int
	for p, s := range m.slots {
		if fence, ok := held[p]; !ok || fence != s.lease.Fence {
			lost = append(lost, p)
		}
	}
	if len(lost) > 0 {
		m.log.Warn("projection: partition leases lost", "partitions", lost)
		m.stop(lost, false)
	}

	// The store may list us as owner of partitions we no longer run: a
	// worker reported ErrLeaseLost, or did not stop in time. Renew just
	// extended those, so release them or nobody would ever claim them.
	for p, fence := range held {
		if _, ok := m.slots[p]; ok {
			continue
		}
		orphan := Lease{Projection: m.name, Partition: p, Member: m.member, Fence: fence}
		if err := m.store.Release(ctx, orphan); err != nil {
			m.log.Error("projection: releasing orphaned lease failed", "partition", p, "err", err)
		}
	}

	if extra := len(m.slots) - target; extra > 0 {
		owned := m.owned()
		sort.Sort(sort.Reverse(sort.IntSlice(owned)))
		m.log.Info("projection: releasing partitions", "partitions", owned[:extra], "live", live, "target", target)
		m.stop(owned[:extra], true)
	}

	if want := target - len(m.slots); want > 0 {
		leases, err := m.store.Claim(ctx, m.name, m.member, want, m.ttl)
		if err != nil {
			m.log.Error("projection: claiming partitions failed", "err", err)
			return
		}
		for _, l := range leases {
			m.start(ctx, l)
		}
		if len(leases) > 0 {
			m.changed()
		}
	}
}

// checkRenewDeadline stops every worker when renewing has failed for a full
// TTL: another instance may own the partitions by now. Their commits would
// fail on the fence anyway; this only stops wasted work.
func (m *leaseManager) checkRenewDeadline() {
	if len(m.slots) > 0 && time.Since(m.lastRenew) > m.ttl {
		m.log.Warn("projection: could not renew leases within their TTL, stopping all partitions")
		m.stop(m.owned(), false)
	}
}

func (m *leaseManager) start(parent context.Context, l Lease) {
	ctx, cancel := context.WithCancel(parent)
	s := &slot{lease: l, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.slots[l.Partition] = s
	m.mu.Unlock()
	go m.supervise(ctx, s)
}

// supervise runs the partition's work until the slot is stopped or the work
// reports the lease lost. Work that fails for another reason is restarted
// after a growing delay; it reloads its checkpoint, so it resumes from the
// last committed batch.
func (m *leaseManager) supervise(ctx context.Context, s *slot) {
	defer close(s.done)
	delay := retryMin
	for {
		err := m.runWork(ctx, s.lease)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, ErrLeaseLost) {
			m.log.Warn("projection: partition taken over by another instance", "partition", s.lease.Partition)
			select {
			case m.lost <- s:
			case <-ctx.Done():
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(2*delay, retryMax)
	}
}

const (
	retryMin = 500 * time.Millisecond
	retryMax = 30 * time.Second
)

func (m *leaseManager) runWork(ctx context.Context, l Lease) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("projection %s: partition %d panicked: %v", m.name, l.Partition, r)
		}
	}()
	return m.work(ctx, l)
}

// stop cancels the given partitions' work, waits for it, and optionally
// releases their leases. A lease is released only after its work stopped,
// so two instances never run a partition at once. Work that does not stop
// in time keeps its lease until it expires.
func (m *leaseManager) stop(partitions []int, release bool) {
	stopping := make([]*slot, 0, len(partitions))
	for _, p := range partitions {
		if s, ok := m.slots[p]; ok {
			s.cancel()
			stopping = append(stopping, s)
		}
	}

	deadline := time.After(m.stopWait)
	for _, s := range stopping {
		stopped := true
		select {
		case <-s.done:
		case <-deadline:
			stopped = false
		}
		m.mu.Lock()
		delete(m.slots, s.lease.Partition)
		m.mu.Unlock()

		if !stopped {
			m.log.Warn("projection: partition did not stop in time, leaving its lease to expire", "partition", s.lease.Partition)
			continue
		}
		if release {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := m.store.Release(ctx, s.lease); err != nil {
				m.log.Error("projection: releasing lease failed, it will expire", "partition", s.lease.Partition, "err", err)
			}
			cancel()
		}
	}
	if len(stopping) > 0 {
		m.changed()
	}
}

func (m *leaseManager) forget(s *slot) {
	m.mu.Lock()
	if cur, ok := m.slots[s.lease.Partition]; ok && cur == s {
		delete(m.slots, s.lease.Partition)
	}
	m.mu.Unlock()
	m.changed()
}

func (m *leaseManager) shutdown() {
	m.stop(m.owned(), true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Leave(ctx, m.name, m.member); err != nil {
		m.log.Error("projection: leaving failed", "err", err)
	}
}

func (m *leaseManager) changed() {
	if m.onChange != nil {
		m.onChange(m.owned())
	}
}
