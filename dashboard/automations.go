package dashboard

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/terraskye/eventsourcing/automation"
)

// The states the page shows for an automation, worst first.
const (
	statePaused  = "paused"  // paused through the store, on every instance
	stateWorking = "working" // an instance is working claimed items
	stateIdle    = "idle"    // the instances found nothing to do
)

var automationRank = map[string]int{
	stateUnreadable: 0,
	stateStalled:    1,
	statePaused:     2,
	stateNotStarted: 3,
	stateWaiting:    4,
	stateWorking:    5,
	stateIdle:       6,
}

// automationView is one automation as the page shows it: what its store
// knows of it across every instance.
type automationView struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Paused   bool   `json:"paused"`
	CanPause bool   `json:"canPause"` // the page offers pause and resume

	// Totals over the live instances, each counted since it started.
	Processed uint64 `json:"processed"`
	Failed    uint64 `json:"failed"`
	Deferred  uint64 `json:"deferred"`

	WaitingFor string `json:"waitingFor,omitempty"` // a dependency an instance waits for
	LastError  string `json:"lastError,omitempty"`  // a stalled instance's error
	Error      string `json:"error,omitempty"`      // the store could not be read

	Instances []automationInstanceView `json:"instances"`
	Segments  []automationSegmentView  `json:"segments"`
}

// automationInstanceView is one live instance of an automation.
type automationInstanceView struct {
	Owner      string `json:"owner"`
	State      string `json:"state"`
	WaitingFor string `json:"waitingFor,omitempty"`
	Processed  uint64 `json:"processed"`
	Failed     uint64 `json:"failed"`
	Deferred   uint64 `json:"deferred"`
	LastError  string `json:"lastError,omitempty"`
	Segments   int    `json:"segments"` // segments it holds
	AgeMs      int64  `json:"ageMs"`
}

// automationSegmentView is one segment of an automation.
type automationSegmentView struct {
	Key      string `json:"key"`
	Group    string `json:"group,omitempty"`
	Owner    string `json:"owner"`
	PausedMs int64  `json:"pausedMs"` // left of a deferral; 0 when not paused
}

// phaseState is the state the page shows for an instance in phase p.
func phaseState(p automation.Phase) string {
	switch p {
	case automation.Stalled:
		return stateStalled
	case automation.Paused:
		return statePaused
	case automation.Working:
		return stateWorking
	case automation.Idle:
		return stateIdle
	default: // Starting, Waiting
		return stateWaiting
	}
}

// automationViewOf turns what the store knows of an automation into what
// the page shows. It takes the worst state of its instances; a pause in
// the store outranks them, since it is what an operator set.
func automationViewOf(name string, paused bool, statuses []automation.Status, segments []automation.SegmentState) automationView {
	v := automationView{
		Name:      name,
		State:     stateNotStarted,
		Paused:    paused,
		Instances: make([]automationInstanceView, 0, len(statuses)),
		Segments:  make([]automationSegmentView, 0, len(segments)),
	}
	held := map[string]int{}
	for _, s := range segments {
		if s.Owner != "" {
			held[s.Owner]++
		}
		v.Segments = append(v.Segments, automationSegmentView{
			Key: s.Key, Group: s.Group, Owner: s.Owner, PausedMs: s.PausedFor.Milliseconds(),
		})
	}
	for i, st := range statuses {
		iv := automationInstanceView{
			Owner:      st.Owner,
			State:      phaseState(st.Phase),
			WaitingFor: st.WaitingFor,
			Processed:  st.Processed,
			Failed:     st.Failed,
			Deferred:   st.Deferred,
			LastError:  st.LastError,
			Segments:   held[st.Owner],
			AgeMs:      st.Age.Milliseconds(),
		}
		v.Instances = append(v.Instances, iv)
		v.Processed += st.Processed
		v.Failed += st.Failed
		v.Deferred += st.Deferred
		if i == 0 || automationRank[iv.State] < automationRank[v.State] {
			v.State = iv.State
		}
		if iv.State == stateStalled && v.LastError == "" {
			v.LastError = st.LastError
		}
		if st.WaitingFor != "" && v.WaitingFor == "" {
			v.WaitingFor = st.WaitingFor
		}
	}
	if paused {
		v.State = statePaused
	}
	return v
}

// readAutomations reads every shown automation from the automation store.
func (d *Dashboard) readAutomations(ctx context.Context) []automationView {
	out := make([]automationView, 0, len(d.automationNames))
	for _, name := range d.automationNames {
		readCtx, cancel := context.WithTimeout(ctx, readTimeout)
		paused, err := d.automations.Paused(readCtx, name)
		statuses, statErr := d.automations.LoadStatuses(readCtx, name)
		segments, segErr := d.automations.LoadSegments(readCtx, name)
		cancel()
		err = errors.Join(err, statErr, segErr)
		v := automationViewOf(name, paused, statuses, segments)
		v.CanPause = d.pause
		if err != nil {
			v.State = stateUnreadable
			v.Error = err.Error()
		}
		out = append(out, v)
	}
	return out
}

// servePause pauses or resumes a shown automation through its store, so it
// takes effect on every instance, wherever it runs.
func (d *Dashboard) servePause(paused bool) http.HandlerFunc {
	verb := "Resuming"
	if paused {
		verb = "Pausing"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if !d.pause {
			writeError(w, http.StatusNotFound, "Pausing automations is turned off for this dashboard.")
			return
		}
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "Automations can only be paused from the dashboard page.")
			return
		}
		name := r.PathValue("name")
		if !slices.Contains(d.automationNames, name) {
			writeError(w, http.StatusNotFound, name+" is not shown on this dashboard.")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
		defer cancel()
		if err := d.automations.SetPaused(ctx, name, paused); err != nil {
			writeError(w, http.StatusInternalServerError, verb+" "+name+" failed: "+err.Error())
			return
		}
		d.hub.poke()
		msg := name + " resumed. Every instance claims work again at its next poll."
		if paused {
			msg = name + " paused. Every instance stops claiming at its next poll; items already running finish."
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": msg})
	}
}
