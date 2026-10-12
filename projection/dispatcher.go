package projection

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/terraskye/eventsourcing"
)

// dispatcher reads the global log once per instance and routes every event
// to the partition it belongs to, if this instance runs that partition.
//
// Partitions register when they start and unregister when they stop. Reading
// starts at the lowest checkpoint of the registered partitions; when a
// partition registers behind the current read position (it just moved here,
// or restarted after a failure), reading starts over from its checkpoint,
// and every other partition skips what it has already received.
type dispatcher struct {
	reader     eventsourcing.GlobalReader
	opts       eventsourcing.ReadOptions
	partitions int
	key        PartitionKey
	poll       time.Duration
	nudge      <-chan struct{}
	buffer     int
	log        *slog.Logger

	head atomic.Uint64 // head of the log as of the last read

	mu      sync.Mutex
	routes  map[int]*route
	reading bool   // readPos is valid
	restart bool   // a partition registered behind readPos
	readPos uint64 // everything up to here has been routed
	wake    chan struct{}
}

type route struct {
	ch   chan item
	done chan struct{} // closed by unregister
	pos  uint64        // last position delivered (guarded by dispatcher.mu)
}

// item is an event for the partition, or, when env is nil, a progress
// marker: every event up to progress has been routed and none of the rest
// belong to the partition. live reports that the read reached the head.
type item struct {
	env      *eventsourcing.Envelope
	progress uint64
	live     bool
}

func newDispatcher(reader eventsourcing.GlobalReader, cfg config, nudge <-chan struct{}, log *slog.Logger) *dispatcher {
	return &dispatcher{
		reader:     reader,
		opts:       cfg.read,
		partitions: cfg.partitions,
		key:        cfg.partitionKey,
		poll:       cfg.pollInterval,
		nudge:      nudge,
		buffer:     max(cfg.read.Limit, 1) * 2,
		log:        log,
		routes:     make(map[int]*route),
		wake:       make(chan struct{}, 1),
	}
}

func (d *dispatcher) signal() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *dispatcher) register(p int, from uint64) *route {
	r := &route{ch: make(chan item, d.buffer), done: make(chan struct{}), pos: from}
	d.mu.Lock()
	d.routes[p] = r
	if d.reading && from < d.readPos {
		d.restart = true
	}
	d.mu.Unlock()
	d.signal()
	return r
}

func (d *dispatcher) unregister(p int, r *route) {
	d.mu.Lock()
	if d.routes[p] == r {
		delete(d.routes, p)
	}
	d.mu.Unlock()
	close(r.done)
}

func (d *dispatcher) run(ctx context.Context) {
	var notify <-chan struct{}
	if n, ok := d.reader.(eventsourcing.Notifier); ok {
		if ch, err := n.Notify(ctx); err == nil {
			notify = ch
		}
	}

	delay := retryMin
	for {
		// Drain a pending wake-up before looking at the routes: a
		// registration after this point signals again and is not lost.
		select {
		case <-d.wake:
		default:
		}

		d.mu.Lock()
		if len(d.routes) == 0 {
			d.reading = false
			d.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-d.wake:
				continue
			}
		}
		if !d.reading || d.restart {
			first := true
			for _, r := range d.routes {
				if first || r.pos < d.readPos {
					d.readPos, first = r.pos, false
				}
			}
			d.reading, d.restart = true, false
		}
		after := d.readPos
		d.mu.Unlock()

		res, err := d.reader.ReadAll(ctx, after, d.opts)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			d.log.Error("projection: reading the log failed, retrying", "err", err, "after", after, "delay", delay)
			if !sleep(ctx, delay) {
				return
			}
			delay = min(2*delay, retryMax)
			continue
		}
		delay = retryMin

		if res.AtEnd {
			d.head.Store(max(d.head.Load(), res.Next))
		} else if h, err := d.reader.Head(ctx); err == nil {
			d.head.Store(max(d.head.Load(), h))
		}

		if !d.route(ctx, res) {
			continue // a partition registered behind us; start over
		}
		if !res.AtEnd {
			continue
		}

		timer := time.NewTimer(d.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-d.wake:
		case <-d.nudge:
		case _, ok := <-notify:
			if !ok {
				notify = nil
			}
		case <-timer.C:
		}
		timer.Stop()
	}
}

// route delivers one page. It returns false when a restart was requested
// while routing, in which case the rest of the page is dropped.
func (d *dispatcher) route(ctx context.Context, res eventsourcing.ReadResult) bool {
	for _, env := range res.Events {
		p := PartitionOf(d.key(env), d.partitions)

		d.mu.Lock()
		if d.restart {
			d.mu.Unlock()
			return false
		}
		d.readPos = env.GlobalVersion
		r := d.routes[p]
		deliver := r != nil && r.pos < env.GlobalVersion
		if deliver {
			r.pos = env.GlobalVersion
		}
		d.mu.Unlock()

		if deliver && !d.send(ctx, r, item{env: env}) {
			return false
		}
	}

	d.mu.Lock()
	if d.restart {
		d.mu.Unlock()
		return false
	}
	d.readPos = res.Next
	var targets []*route
	for _, r := range d.routes {
		// A partition whose checkpoint is past this page hears nothing
		// until reading gets there.
		if r.pos <= res.Next {
			r.pos = res.Next
			targets = append(targets, r)
		}
	}
	d.mu.Unlock()

	for _, r := range targets {
		if !d.send(ctx, r, item{progress: res.Next, live: res.AtEnd}) {
			return false
		}
	}
	return true
}

func (d *dispatcher) send(ctx context.Context, r *route, it item) bool {
	select {
	case r.ch <- it:
	case <-r.done: // the partition stopped; its next owner reads from its checkpoint
	case <-ctx.Done():
		return false
	}
	return true
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
