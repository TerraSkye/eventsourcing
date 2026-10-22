package dashboard_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/dashboard"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

type ticked struct{ ID string }

func (e *ticked) AggregateID() string { return e.ID }
func (e *ticked) EventType() string   { return "dashboard_test.Ticked" }

func init() { eventsourcing.RegisterEvent(&ticked{}) }

// fixture runs one projection, "ticks", over a memory event store.
type fixture struct {
	events *memory.EventStore
	store  projection.Store
	runner *projection.Runner
	resets atomic.Int64
}

func newFixture(t *testing.T, withReset bool) *fixture {
	t.Helper()
	f := &fixture{events: memory.NewEventStore(1024), store: memory.NewProjectionStore()}
	def := projection.Definition{
		Name:         "ticks",
		Handler:      eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error { return nil }),
		EventTypes:   []string{"dashboard_test.Ticked"},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
	}
	if withReset {
		def.Reset = func(context.Context) error { f.resets.Add(1); return nil }
	}
	f.runner = projection.NewRunner(def, f.events, f.store)

	envs := make([]eventsourcing.Envelope, 5)
	for i := range envs {
		envs[i] = eventsourcing.Envelope{EventID: uuid.New(), StreamID: "s", Event: &ticked{ID: "s"}, Version: uint64(i + 1)}
	}
	res, err := f.events.Save(t.Context(), envs, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = f.runner.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer waitCancel()
	if err := f.runner.WaitUntil(waitCtx, res.GlobalPosition); err != nil {
		t.Fatalf("WaitUntil: %v", err)
	}
	return f
}

type snapshot struct {
	Projections []struct {
		Name     string `json:"name"`
		State    string `json:"state"`
		Position uint64 `json:"position"`
		Local    bool   `json:"local"`
		CanReset bool   `json:"canReset"`
	} `json:"projections"`
}

func getSnapshot(t *testing.T, h http.Handler) snapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, "/api/snapshot", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/snapshot = %d: %s", rec.Code, rec.Body)
	}
	var s snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	return s
}

func TestSnapshot_ShowsRunnersAndFollowedProjections(t *testing.T) {
	f := newFixture(t, true)
	d := dashboard.New(f.store, dashboard.WithRunners(f.runner), dashboard.WithProjections("invoices"), dashboard.WithReset())

	s := getSnapshot(t, d)
	if len(s.Projections) != 2 {
		t.Fatalf("projections = %+v, want ticks and invoices", s.Projections)
	}
	ticks, invoices := s.Projections[0], s.Projections[1]
	if ticks.Name != "ticks" || ticks.State != "live" || ticks.Position != 5 || !ticks.Local || !ticks.CanReset {
		t.Errorf("ticks = %+v, want live at 5, local, with a rebuild button", ticks)
	}
	if invoices.Name != "invoices" || invoices.State != "not_started" || !invoices.CanReset {
		t.Errorf("invoices = %+v, want not_started with a rebuild button: the store takes the request", invoices)
	}
}

func TestPage_IsServedAtTheRoot(t *testing.T) {
	d := dashboard.New(memory.NewProjectionStore())
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, localRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Projections</title>") {
		t.Fatalf("GET / = %d, want the page", rec.Code)
	}
}

func TestPage_RedirectsToTheTrailingSlash(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/dashboard/", http.StripPrefix("/dashboard", dashboard.New(memory.NewProjectionStore())))
	mux.Handle("/dashboard", http.StripPrefix("/dashboard", dashboard.New(memory.NewProjectionStore())))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, localRequest(http.MethodGet, "/dashboard", nil))
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusMovedPermanently || loc != "/dashboard/" {
		t.Fatalf("GET /dashboard = %d to %q, want a redirect to /dashboard/", rec.Code, loc)
	}
}

// TestPage_RedirectKeepsTheQuery covers a redirect to the trailing slash of
// a URL with a query: the slash goes on the path, so the redirect does not
// loop.
func TestPage_RedirectKeepsTheQuery(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/dashboard", http.StripPrefix("/dashboard", dashboard.New(memory.NewProjectionStore())))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, localRequest(http.MethodGet, "/dashboard?tab=ticks", nil))
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusMovedPermanently || loc != "/dashboard/?tab=ticks" {
		t.Fatalf("GET /dashboard?tab=ticks = %d to %q, want a redirect to /dashboard/?tab=ticks", rec.Code, loc)
	}
}

// TestHost_RefusesNamesNotAllowed covers DNS rebinding: a page on a site
// whose name resolves to the dashboard's address sends that name as Host,
// and is refused unless the name is allowed.
func TestHost_RefusesNamesNotAllowed(t *testing.T) {
	tests := []struct {
		host    string
		allowed []string
		want    int
	}{
		{host: "localhost:8080", want: http.StatusOK},
		{host: "127.0.0.1:8080", want: http.StatusOK},
		{host: "[::1]:8080", want: http.StatusOK},
		{host: "10.0.0.7", want: http.StatusOK},
		{host: "attacker.example:8080", want: http.StatusForbidden},
		{host: "ops.example.com", want: http.StatusForbidden},
		{host: "ops.example.com", allowed: []string{"ops.example.com"}, want: http.StatusOK},
		{host: "OPS.example.com:443", allowed: []string{"ops.example.com"}, want: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			d := dashboard.New(memory.NewProjectionStore(), dashboard.WithAllowedHosts(tt.allowed...))
			req := httptest.NewRequest(http.MethodGet, "/api/snapshot", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			d.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("GET /api/snapshot with Host %q, allowed %v = %d, want %d", tt.host, tt.allowed, rec.Code, tt.want)
			}
		})
	}
}

// TestEvents_StreamsSnapshots covers the live updates the page listens to.
func TestEvents_StreamsSnapshots(t *testing.T) {
	f := newFixture(t, false)
	srv := httptest.NewServer(dashboard.New(f.store, dashboard.WithRunners(f.runner), dashboard.WithInterval(20*time.Millisecond)))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	lines, updates := bufio.NewScanner(res.Body), 0
	for updates < 2 && lines.Scan() {
		data, ok := strings.CutPrefix(lines.Text(), "data: ")
		if !ok {
			continue
		}
		var s snapshot
		if err := json.Unmarshal([]byte(data), &s); err != nil || len(s.Projections) != 1 || s.Projections[0].Name != "ticks" {
			t.Fatalf("update = %s (%v), want a snapshot of ticks", data, err)
		}
		updates++
	}
	if updates < 2 {
		t.Fatalf("got %d updates before the stream ended (%v), want a stream of them", updates, lines.Err())
	}
}

// localRequest is an incoming request for localhost, which the dashboard
// answers without WithAllowedHosts.
func localRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "localhost:8080"
	return req
}

func rebuild(d http.Handler, name string, header map[string]string) *httptest.ResponseRecorder {
	req := localRequest(http.MethodPost, "/api/projections/"+name+"/rebuild", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec
}

var fromThePage = map[string]string{"Sec-Fetch-Site": "same-origin"}

// waitResets waits until the fixture's Reset ran n times.
func waitResets(t *testing.T, f *fixture, n int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.resets.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("Reset ran %d times within 3s, want %d", f.resets.Load(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRebuild_ResetsTheProjection(t *testing.T) {
	f := newFixture(t, true)
	d := dashboard.New(f.store, dashboard.WithRunners(f.runner), dashboard.WithReset())

	if rec := rebuild(d, "ticks", fromThePage); rec.Code != http.StatusAccepted {
		t.Fatalf("rebuild = %d: %s", rec.Code, rec.Body)
	}
	waitResets(t, f, 1)
}

// TestRebuild_ProjectionRunElsewhere covers a dashboard in a process that
// does not run the projection, such as a web binary in front of a worker.
func TestRebuild_ProjectionRunElsewhere(t *testing.T) {
	f := newFixture(t, true)
	web := dashboard.New(f.store, dashboard.WithProjections("ticks"), dashboard.WithReset())

	if s := getSnapshot(t, web); !s.Projections[0].CanReset {
		t.Fatalf("snapshot = %+v, want a rebuild button", s.Projections[0])
	}
	if rec := rebuild(web, "ticks", fromThePage); rec.Code != http.StatusAccepted {
		t.Fatalf("rebuild = %d: %s", rec.Code, rec.Body)
	}
	waitResets(t, f, 1)
}

func TestRebuild_IsOffWithoutWithReset(t *testing.T) {
	f := newFixture(t, true)
	d := dashboard.New(f.store, dashboard.WithRunners(f.runner))

	if rec := rebuild(d, "ticks", fromThePage); rec.Code != http.StatusNotFound {
		t.Errorf("rebuild = %d, want 404 while rebuilding is off", rec.Code)
	}
	if n := f.resets.Load(); n != 0 {
		t.Errorf("Reset ran %d times, want never", n)
	}
}

// TestRebuild_RefusesOtherSites covers that another site cannot rebuild a
// projection through a visitor's browser.
func TestRebuild_RefusesOtherSites(t *testing.T) {
	f := newFixture(t, true)
	d := dashboard.New(f.store, dashboard.WithRunners(f.runner), dashboard.WithReset())

	for _, header := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site"},
		{"Origin": "https://elsewhere.example"},
	} {
		if rec := rebuild(d, "ticks", header); rec.Code != http.StatusForbidden {
			t.Errorf("rebuild with %v = %d, want 403", header, rec.Code)
		}
	}
	if n := f.resets.Load(); n != 0 {
		t.Errorf("Reset ran %d times, want never", n)
	}
}

func TestRebuild_ExplainsWhyItCannot(t *testing.T) {
	f := newFixture(t, false) // no Reset
	d := dashboard.New(f.store, dashboard.WithRunners(f.runner), dashboard.WithProjections("invoices"), dashboard.WithReset())

	if s := getSnapshot(t, d); s.Projections[0].CanReset {
		t.Errorf("ticks = %+v, want no rebuild button without a Reset", s.Projections[0])
	}
	if rec := rebuild(d, "ticks", fromThePage); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no Reset") {
		t.Errorf("rebuild without Reset = %d %s, want 409 naming the missing Reset", rec.Code, rec.Body)
	}
	if rec := rebuild(d, "invoices", fromThePage); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "nothing to rebuild") {
		t.Errorf("rebuild of a projection no runner started = %d %s, want 409", rec.Code, rec.Body)
	}
	if rec := rebuild(d, "orders", fromThePage); rec.Code != http.StatusNotFound {
		t.Errorf("rebuild of a projection the dashboard does not show = %d, want 404", rec.Code)
	}
}
