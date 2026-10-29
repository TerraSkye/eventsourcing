package dashboard

import (
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

// The states the page shows, worst first. A projection takes the worst
// state of its segments.
const (
	stateUnreadable       = "unreadable"        // the store could not be read
	stateStalled          = "stalled"           // a segment's last attempt failed
	stateSilent           = "silent"            // a segment's owner stopped publishing
	stateRebuildRequested = "rebuild_requested" // a rebuild waits for segment 0's runner
	stateNotStarted       = "not_started"       // no runner has declared the projection
	stateWaiting          = "waiting"           // a segment is starting or unowned
	stateReplaying        = "replaying"         // a segment replays events it processed before
	stateCatchingUp       = "catching_up"       // a segment is behind the head of the log
	stateLive             = "live"              // every segment is at the head of the log
)

var stateRank = map[string]int{
	stateUnreadable:       0,
	stateStalled:          1,
	stateSilent:           2,
	stateRebuildRequested: 3,
	stateNotStarted:       4,
	stateWaiting:          5,
	stateReplaying:        6,
	stateCatchingUp:       7,
	stateLive:             8,
}

// snapshot is what the page receives on every update.
type snapshot struct {
	Title       string           `json:"title"`
	Reset       bool             `json:"reset"`
	Events      bool             `json:"events"` // the page offers the events view
	Chains      bool             `json:"chains"` // events link to their correlation chain
	Projections []projectionView `json:"projections"`
}

// projectionView is one projection as the page shows it.
type projectionView struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Position uint64  `json:"position"` // the lowest segment position
	Head     uint64  `json:"head"`     // the highest head any segment saw
	Behind   uint64  `json:"behind"`   // the largest backlog of any segment
	Rate     float64 `json:"rate"`     // positions processed per second
	ETA      float64 `json:"eta"`      // seconds to catch up; 0 when unknown
	Version  int     `json:"version"`
	Local    bool    `json:"local"`    // run by this program
	CanReset bool    `json:"canReset"` // the page offers a rebuild

	RebuildRequested bool           `json:"rebuildRequested"` // a rebuild is pending
	Error            string         `json:"error,omitempty"`
	Segments         []segmentView  `json:"segments"`
	Instances        []instanceView `json:"instances"` // standby ones included
}

// instanceView is one live instance of a projection as the page shows it.
type instanceView struct {
	Owner    string `json:"owner"`
	Version  int    `json:"version"`
	Segments int    `json:"segments"` // 0 for an instance on standby
	AgeMs    int64  `json:"ageMs"`
}

// instancesView turns a projection's instances into what the page shows.
func instancesView(instances []projection.Instance) []instanceView {
	out := make([]instanceView, 0, len(instances))
	for _, m := range instances {
		out = append(out, instanceView{Owner: m.Owner, Version: m.Version, Segments: m.Segments, AgeMs: m.Age.Milliseconds()})
	}
	return out
}

// segmentView is one segment as the page shows it.
type segmentView struct {
	Segment   int    `json:"segment"`
	State     string `json:"state"`
	Position  uint64 `json:"position"`
	Head      uint64 `json:"head"`
	Replay    uint64 `json:"replayUntil"`
	Owner     string `json:"owner"`
	AgeMs     int64  `json:"ageMs"`
	LastError string `json:"lastError,omitempty"`
	Version   int    `json:"version"`
}

// view turns a projection's statuses into what the page shows.
func view(name string, statuses []projection.Status, claimTTL time.Duration) projectionView {
	v := projectionView{Name: name, State: stateNotStarted, Segments: make([]segmentView, 0, len(statuses))}
	for i, s := range statuses {
		seg := segmentView{
			Segment:   s.Segment,
			State:     segmentState(s, claimTTL),
			Position:  s.Position,
			Head:      s.Head,
			Replay:    s.ReplayUntil,
			Owner:     s.Owner,
			AgeMs:     s.Age.Milliseconds(),
			LastError: s.LastError,
			Version:   s.Version,
		}
		v.Segments = append(v.Segments, seg)

		if i == 0 {
			v.State, v.Position = seg.State, s.Position
		}
		if stateRank[seg.State] < stateRank[v.State] {
			v.State = seg.State
		}
		v.Position = min(v.Position, s.Position)
		v.Head = max(v.Head, s.Head)
		v.Version = max(v.Version, s.Version)
		if s.RebuildRequested {
			v.RebuildRequested = true
			if stateRank[stateRebuildRequested] < stateRank[v.State] {
				v.State = stateRebuildRequested
			}
		}
		// Each segment's backlog is against the head it last read itself.
		// Comparing the lowest position with the highest head would only
		// measure when the segments last read, and call a live projection
		// behind.
		if s.Head > s.Position {
			v.Behind = max(v.Behind, s.Head-s.Position)
		}
	}
	return v
}

// segmentState is the state the page shows for one segment. A segment is
// silent once its status is older than its claim's ttl: the one the store
// reports, as [projection.Status.IsLive] judges it, or else claimTTL.
func segmentState(s projection.Status, claimTTL time.Duration) string {
	if s.ClaimTTL > 0 {
		claimTTL = s.ClaimTTL
	}
	if s.Owner != "" && s.Age > claimTTL && s.Phase != projection.Standby {
		return stateSilent
	}
	switch s.Phase {
	case projection.Stalled:
		return stateStalled
	case projection.CatchingUp:
		if s.Position < s.ReplayUntil {
			return stateReplaying
		}
		return stateCatchingUp
	case projection.Live:
		return stateLive
	default: // Starting, Standby
		return stateWaiting
	}
}

// rate estimates how many positions per second a projection processes,
// over the last few seconds, so a single fast batch does not make the page
// promise a projection is about to be live.
type rate struct {
	samples   []sample
	perSecond float64
}

type sample struct {
	position uint64
	at       time.Time
}

const (
	// rateWindow is how far back the estimate looks.
	rateWindow = 5 * time.Second
	// rateMinSpan is how much history the first estimate needs.
	rateMinSpan = time.Second
)

func (r *rate) observe(position uint64, at time.Time) {
	if n := len(r.samples); n > 0 && position < r.samples[n-1].position {
		// A rebuild moved the projection back; start measuring again.
		r.samples, r.perSecond = nil, 0
	}
	r.samples = append(r.samples, sample{position, at})
	// Keep one sample at or before the window's start, so the estimate
	// spans the whole window.
	for len(r.samples) > 2 && at.Sub(r.samples[1].at) >= rateWindow {
		r.samples = r.samples[1:]
	}
	first := r.samples[0]
	if span := at.Sub(first.at); span >= rateMinSpan {
		r.perSecond = float64(position-first.position) / span.Seconds()
	}
}
