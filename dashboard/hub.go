package dashboard

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	// readTimeout bounds one read of a projection's statuses, so a hung
	// store delays the page instead of freezing it.
	readTimeout = 5 * time.Second

	// minGap is the least time between two reads. A runner pokes the hub
	// on every commit, which during a catch-up is many times a second.
	minGap = 100 * time.Millisecond
)

// hub reads the statuses for every open page: once per interval, however
// many pages are open, and only while at least one is.
type hub struct {
	d *Dashboard

	nudge chan struct{} // buffered(1): read now

	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	stop   context.CancelFunc // stops the reader; nil when none runs
	latest []byte             // the last snapshot sent, for a page that opens
	rates  map[string]*rate
}

func newHub(d *Dashboard) *hub {
	return &hub{
		d:     d,
		nudge: make(chan struct{}, 1),
		subs:  map[chan []byte]struct{}{},
		rates: map[string]*rate{},
	}
}

// subscribe returns a channel that receives every snapshot from now on,
// starting with the latest one. The first subscriber starts the reader.
func (h *hub) subscribe() chan []byte {
	ch := make(chan []byte, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	if h.latest != nil {
		ch <- h.latest
	}
	if h.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.stop = cancel
		// Rates measured before the dashboard went idle say nothing now.
		h.rates = map[string]*rate{}
		go h.run(ctx)
	}
	return ch
}

// unsubscribe stops sending to ch. The last subscriber stops the reader.
func (h *hub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
	if len(h.subs) == 0 && h.stop != nil {
		h.stop()
		h.stop = nil
		h.latest = nil
	}
}

// poke asks the reader to read now. Pokes coalesce.
func (h *hub) poke() {
	select {
	case h.nudge <- struct{}{}:
	default:
	}
}

// run reads at once, then every interval and on pokes, until ctx ends.
// Reads are at least minGap apart: a poke that comes sooner is answered
// when that gap has passed.
func (h *hub) run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	var earliest time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.nudge:
			if wait := time.Until(earliest); wait > 0 {
				t.Reset(wait)
				continue
			}
		}
		earliest = time.Now().Add(min(minGap, h.d.interval))
		snap := h.read(ctx, true)
		b, err := json.Marshal(snap)
		if err == nil && ctx.Err() == nil {
			h.publish(ctx, b)
		}
		t.Reset(h.d.interval)
	}
}

// publish sends b to every subscriber. A subscriber still holding an
// older snapshot gets the new one instead: pages only need the latest.
func (h *hub) publish(ctx context.Context, b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil {
		return // stopped mid-read; a newer reader owns the subscribers
	}
	h.latest = b
	for ch := range h.subs {
		select {
		case <-ch:
		default:
		}
		ch <- b
	}
}

// read takes a snapshot of every projection. With track, it also feeds the
// rate estimates; a one-off read only uses them.
func (h *hub) read(ctx context.Context, track bool) snapshot {
	d := h.d
	snap := snapshot{Title: d.title, Reset: d.reset, Projections: make([]projectionView, 0, len(d.names))}
	for _, name := range d.names {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		statuses, err := d.store.LoadStatuses(readCtx, name)
		instances, instErr := d.store.LoadInstances(readCtx, name)
		cancel()
		if err == nil {
			err = instErr
		}
		now := time.Now()

		runner, local := d.runners[name]
		v := view(name, statuses, d.claimTTL)
		v.Instances = instancesView(instances)
		v.Local = local
		// A runner here says whether it has a Reset; for one elsewhere,
		// only its own runner knows, and the request shows it.
		v.CanReset = d.reset && (!local || runner.CanRebuild())
		if err != nil {
			v.State = stateUnreadable
			v.Error = err.Error()
		}

		h.mu.Lock()
		r := h.rates[name]
		if r == nil {
			r = &rate{}
			if track {
				h.rates[name] = r
			}
		}
		if track && err == nil && len(statuses) > 0 {
			r.observe(v.Position, now)
		}
		v.Rate = r.perSecond
		h.mu.Unlock()

		if v.Rate > 0 && v.Behind > 0 {
			v.ETA = float64(v.Behind) / v.Rate
		}
		snap.Projections = append(snap.Projections, v)
	}
	return snap
}
