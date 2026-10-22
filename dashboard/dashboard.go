package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/terraskye/eventsourcing/projection"
)

const (
	// DefaultInterval is how often an open dashboard reads the statuses,
	// unless [WithInterval] says otherwise.
	DefaultInterval = 500 * time.Millisecond
)

//go:embed static/index.html
var static embed.FS

// Dashboard is an [http.Handler] that serves the dashboard page and its
// updates. Create one with [New].
type Dashboard struct {
	store    projection.Store
	names    []string                      // in display order
	runners  map[string]*projection.Runner // projections this program runs
	reset    bool
	interval time.Duration
	claimTTL time.Duration
	title    string

	mux *http.ServeMux
	hub *hub
}

// Option configures a [Dashboard].
type Option func(*Dashboard)

// WithRunners shows the projections these runners run. Each pushes an
// update as soon as it commits or changes phase.
func WithRunners(runners ...*projection.Runner) Option {
	return func(d *Dashboard) {
		for _, r := range runners {
			d.runners[r.Name()] = r
			d.names = append(d.names, r.Name())
		}
	}
}

// WithProjections shows the projections with these names, as published to
// the store by whichever program runs them.
func WithProjections(names ...string) Option {
	return func(d *Dashboard) { d.names = append(d.names, names...) }
}

// WithReset adds a rebuild button to every projection shown. Only enable it
// where the people who can open the dashboard may wipe a read model.
func WithReset() Option {
	return func(d *Dashboard) { d.reset = true }
}

// WithInterval sets how often the dashboard reads the statuses while a page
// is open. Default [DefaultInterval].
func WithInterval(interval time.Duration) Option {
	return func(d *Dashboard) { d.interval = interval }
}

// WithClaimTTL sets the [projection.Definition.ClaimTTL] the projections run
// with, so the page can show a segment whose runner stopped publishing.
// Default [projection.DefaultClaimTTL].
func WithClaimTTL(ttl time.Duration) Option {
	return func(d *Dashboard) { d.claimTTL = ttl }
}

// WithTitle sets the page's title. Default "Projections".
func WithTitle(title string) Option {
	return func(d *Dashboard) { d.title = title }
}

// New returns a [Dashboard] showing projections whose status is published
// to store.
func New(store projection.Store, opts ...Option) *Dashboard {
	d := &Dashboard{
		store:    store,
		runners:  map[string]*projection.Runner{},
		interval: DefaultInterval,
		claimTTL: projection.DefaultClaimTTL,
		title:    "Projections",
	}
	for _, opt := range opts {
		opt(d)
	}
	if d.interval <= 0 {
		d.interval = DefaultInterval
	}
	if d.claimTTL <= 0 {
		d.claimTTL = projection.DefaultClaimTTL
	}
	d.names = dedupe(d.names)
	d.hub = newHub(d)
	for _, r := range d.runners {
		r.Watch(d.hub.poke)
	}

	d.mux = http.NewServeMux()
	d.mux.HandleFunc("GET /{$}", d.servePage)
	d.mux.HandleFunc("GET /events", d.serveEvents)
	d.mux.HandleFunc("GET /api/snapshot", d.serveSnapshot)
	d.mux.HandleFunc("POST /api/projections/{name}/rebuild", d.serveRebuild)
	return d
}

// ServeHTTP implements [http.Handler].
func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "" {
		// Mounted under http.StripPrefix and opened without the trailing
		// slash: the page's relative links need it.
		http.Redirect(w, r, r.RequestURI+"/", http.StatusMovedPermanently)
		return
	}
	d.mux.ServeHTTP(w, r)
}

func (d *Dashboard) servePage(w http.ResponseWriter, r *http.Request) {
	page, err := static.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'")
	_, _ = w.Write(page)
}

// serveEvents streams a snapshot after every read, as server-sent events.
func (d *Dashboard) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // keep nginx from buffering the stream

	sub := d.hub.subscribe()
	defer d.hub.unsubscribe(sub)

	keepAlive := time.NewTicker(15 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case snap := <-sub:
			if _, err := w.Write([]byte("data: ")); err != nil {
				return
			}
			_, _ = w.Write(snap)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		case <-keepAlive.C:
			// A comment line, so proxies do not close an idle stream.
			if _, err := w.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// serveSnapshot answers with the statuses as of now, for scripts and checks.
func (d *Dashboard) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, d.hub.read(ctx, false))
}

// serveRebuild requests a rebuild of a shown projection through the store,
// so it works wherever the projection runs. It answers once the request is
// recorded; the page shows it pending until the runner that holds segment
// 0 serves it.
func (d *Dashboard) serveRebuild(w http.ResponseWriter, r *http.Request) {
	if !d.reset {
		writeError(w, http.StatusNotFound, "Rebuilding is turned off for this dashboard.")
		return
	}
	if !sameOrigin(r) {
		writeError(w, http.StatusForbidden, "Rebuilds can only be started from the dashboard page.")
		return
	}
	name := r.PathValue("name")
	if !slices.Contains(d.names, name) {
		writeError(w, http.StatusNotFound, name+" is not shown on this dashboard.")
		return
	}
	if runner, ok := d.runners[name]; ok && !runner.CanRebuild() {
		writeError(w, http.StatusConflict, name+" has no Reset in its Definition, so it cannot be rebuilt.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	_, err := d.store.RequestRebuild(ctx, name)
	d.hub.poke()
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"message": "Rebuild of " + name + " requested. The runner holding segment 0 starts it between two batches."})
	case errors.Is(err, projection.ErrUnknownProjection):
		writeError(w, http.StatusConflict, "No runner has started "+name+" yet, so there is nothing to rebuild.")
	default:
		writeError(w, http.StatusInternalServerError, "Requesting a rebuild of "+name+" failed: "+err.Error())
	}
}

// sameOrigin reports whether r comes from the dashboard's own page, or from
// a client that is not a browser. Browsers send Sec-Fetch-Site or Origin
// with every POST, so a page on another site cannot pass.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func dedupe(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}
