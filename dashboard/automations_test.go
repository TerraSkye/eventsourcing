package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/dashboard"
	"github.com/terraskye/eventsourcing/memory"
)

type automationSnapshot struct {
	Title           string `json:"title"`
	ShowAutomations bool   `json:"showAutomations"`
	Automations     []struct {
		Name       string `json:"name"`
		State      string `json:"state"`
		Paused     bool   `json:"paused"`
		CanPause   bool   `json:"canPause"`
		Processed  uint64 `json:"processed"`
		Failed     uint64 `json:"failed"`
		Deferred   uint64 `json:"deferred"`
		WaitingFor string `json:"waitingFor"`
		LastError  string `json:"lastError"`
		Instances  []struct {
			Owner     string `json:"owner"`
			State     string `json:"state"`
			Processed uint64 `json:"processed"`
			Segments  int    `json:"segments"`
		} `json:"instances"`
		Segments []struct {
			Key      string `json:"key"`
			Group    string `json:"group"`
			Owner    string `json:"owner"`
			PausedMs int64  `json:"pausedMs"`
		} `json:"segments"`
	} `json:"automations"`
}

func getAutomations(t *testing.T, h http.Handler) automationSnapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/snapshot = %d: %s", rec.Code, rec.Body)
	}
	var s automationSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return s
}

func pause(d http.Handler, name, action string, header map[string]string) *httptest.ResponseRecorder {
	req := localRequest(http.MethodPost, "/api/automations/"+name+"/"+action, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

// TestAutomations_ShowWhatTheStoreKnows covers an automation as two
// instances publish it: totals over both, the worst state, and who holds
// which segment.
func TestAutomations_ShowWhatTheStoreKnows(t *testing.T) {
	ctx := t.Context()
	store := memory.NewAutomationStore()
	if _, err := store.Acquire(ctx, "sync", "worker-1", []automation.Segment{{Key: "acme", Group: "tenant"}}, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Defer(ctx, "sync", "zed", time.Minute); err != nil {
		t.Fatal(err)
	}
	store.SaveStatus(ctx, "sync", automation.Status{Owner: "worker-1", Phase: automation.Working, Processed: 3, Deferred: 1}, time.Minute)
	store.SaveStatus(ctx, "sync", automation.Status{Owner: "worker-2", Phase: automation.Stalled, Processed: 2, Failed: 1, LastError: "store down"}, time.Minute)

	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithAutomations(store, "sync", "mail"))
	s := getAutomations(t, d)
	if !s.ShowAutomations || s.Title != "Automations" || len(s.Automations) != 2 {
		t.Fatalf("snapshot = %+v, want the automations view titled Automations with sync and mail", s)
	}
	sync, mail := s.Automations[0], s.Automations[1]
	if sync.State != "stalled" || sync.Processed != 5 || sync.Failed != 1 || sync.Deferred != 1 || sync.LastError != "store down" {
		t.Errorf("sync = %+v, want stalled with the totals of both instances and the stalled one's error", sync)
	}
	if sync.CanPause {
		t.Error("sync offers pause without WithPause")
	}
	if len(sync.Instances) != 2 || sync.Instances[0].Owner != "worker-1" || sync.Instances[0].Segments != 1 || sync.Instances[1].Segments != 0 {
		t.Errorf("instances = %+v, want worker-1 holding one segment and worker-2 none", sync.Instances)
	}
	if len(sync.Segments) != 2 {
		t.Fatalf("segments = %+v, want acme and zed", sync.Segments)
	}
	if acme, zed := sync.Segments[0], sync.Segments[1]; acme.Owner != "worker-1" || acme.Group != "tenant" || zed.Owner != "" || zed.PausedMs <= 0 {
		t.Errorf("segments = %+v, want acme held by worker-1 and zed paused", sync.Segments)
	}
	if mail.State != "not_started" || len(mail.Instances) != 0 {
		t.Errorf("mail = %+v, want not_started: no instance published it", mail)
	}
}

// TestAutomations_PauseAndResumeARunner covers the buttons end to end: a
// runner that shares the dashboard's store stops claiming when paused and
// works its list off when resumed.
func TestAutomations_PauseAndResumeARunner(t *testing.T) {
	store := memory.NewAutomationStore()
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithAutomations(store, "work"), dashboard.WithPause())
	if rec := pause(d, "work", "pause", fromThePage); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d: %s", rec.Code, rec.Body)
	}

	list := &todo{pending: map[string]bool{"a": true}}
	r := automation.NewRunner(automation.Definition[string]{
		Name:         "work",
		Projection:   live{},
		Store:        store,
		TodoList:     list,
		Work:         list.complete,
		PollInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = r.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitFor(t, "the runner published Paused", func() bool {
		s := getAutomations(t, d)
		a := s.Automations[0]
		return a.State == "paused" && a.Paused && a.CanPause && len(a.Instances) == 1 && a.Instances[0].State == "paused"
	})
	if list.left() != 1 {
		t.Fatal("a paused runner worked an item")
	}

	if rec := pause(d, "work", "resume", fromThePage); rec.Code != http.StatusOK {
		t.Fatalf("resume = %d: %s", rec.Code, rec.Body)
	}
	waitFor(t, "the item worked after resuming", func() bool { return list.left() == 0 })
	waitFor(t, "the runner published Idle", func() bool {
		a := getAutomations(t, d).Automations[0]
		return a.State == "idle" && !a.Paused && a.Processed == 1
	})
}

func TestAutomations_PauseIsOffWithoutWithPause(t *testing.T) {
	store := memory.NewAutomationStore()
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithAutomations(store, "work"))
	if rec := pause(d, "work", "pause", fromThePage); rec.Code != http.StatusNotFound {
		t.Errorf("pause = %d, want 404 while pausing is off", rec.Code)
	}
	if p, _ := store.Paused(t.Context(), "work"); p {
		t.Error("the automation was paused")
	}
}

func TestAutomations_PauseRefusesOtherSitesAndUnknownNames(t *testing.T) {
	store := memory.NewAutomationStore()
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithAutomations(store, "work"), dashboard.WithPause())
	for _, header := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://elsewhere.example"},
	} {
		if rec := pause(d, "work", "pause", header); rec.Code != http.StatusForbidden {
			t.Errorf("pause with %v = %d, want 403", header, rec.Code)
		}
	}
	if rec := pause(d, "other", "pause", fromThePage); rec.Code != http.StatusNotFound {
		t.Errorf("pause of an automation not shown = %d, want 404", rec.Code)
	}
	if p, _ := store.Paused(t.Context(), "work"); p {
		t.Error("the automation was paused")
	}
}

// todo is a plain todo list of string IDs.
type todo struct {
	mu      sync.Mutex
	pending map[string]bool
}

func (l *todo) Claim(_ context.Context, req automation.ClaimRequest) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for id := range l.pending {
		if len(out) < req.MaxPerKey {
			out = append(out, id)
		}
	}
	return out, nil
}

func (l *todo) complete(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, id)
	return nil
}

func (l *todo) left() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

// live is a dependency that is always live.
type live struct{}

func (live) Name() string                         { return "todo" }
func (live) IsLive(context.Context) (bool, error) { return true, nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
