package dashboard_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/dashboard"
	"github.com/terraskye/eventsourcing/memory"
)

type tocked struct{ ID string }

func (e *tocked) AggregateID() string { return e.ID }
func (e *tocked) EventType() string   { return "dashboard_test.Tocked" }

func init() { eventsourcing.RegisterEvent(&tocked{}) }

type eventsPage struct {
	Events []struct {
		Position      uint64          `json:"position"`
		EventID       string          `json:"eventId"`
		StreamID      string          `json:"streamId"`
		Version       uint64          `json:"version"`
		Type          string          `json:"type"`
		CorrelationID string          `json:"correlationId"`
		CausationID   string          `json:"causationId"`
		Payload       json.RawMessage `json:"payload"`
	} `json:"events"`
	Head    uint64 `json:"head"`
	Older   uint64 `json:"older"`
	Scanned uint64 `json:"scanned"`
}

func (p eventsPage) positions() []uint64 {
	out := make([]uint64, len(p.Events))
	for i, e := range p.Events {
		out[i] = e.Position
	}
	return out
}

// appendEvent saves one event to stream, of type tocked when tock is set,
// in chain correlation caused by causation, and returns its ID.
func appendEvent(t *testing.T, store *memory.EventStore, stream string, tock bool, correlation, causation string) string {
	t.Helper()
	var ev eventsourcing.Event = &ticked{ID: stream}
	if tock {
		ev = &tocked{ID: stream}
	}
	env := eventsourcing.Envelope{
		EventID: uuid.New(), StreamID: stream, Event: ev,
		CorrelationID: correlation, CausationID: causation,
	}
	if _, err := store.Save(t.Context(), []eventsourcing.Envelope{env}, eventsourcing.Any{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return env.EventID.String()
}

func getJSON(t *testing.T, h http.Handler, target string, want int, v any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, localRequest(http.MethodGet, target, nil))
	if rec.Code != want {
		t.Fatalf("GET %s = %d, want %d: %s", target, rec.Code, want, rec.Body)
	}
	if v != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
	}
}

func TestEventList_PagesNewestFirst(t *testing.T) {
	store := memory.NewEventStore()
	for i := range 7 {
		appendEvent(t, store, fmt.Sprintf("s-%d", i), false, "", "")
	}
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithEvents(store))

	var seen []uint64
	before := uint64(0)
	for range 10 {
		var page eventsPage
		getJSON(t, d, fmt.Sprintf("/api/events?limit=3&before=%d", before), http.StatusOK, &page)
		if page.Head != 7 {
			t.Errorf("head = %d, want 7", page.Head)
		}
		seen = append(seen, page.positions()...)
		if page.Older == 0 {
			break
		}
		before = page.Older
	}
	if want := []uint64{7, 6, 5, 4, 3, 2, 1}; !slices.Equal(seen, want) {
		t.Errorf("paging with Older visited %v, want %v", seen, want)
	}
}

func TestEventList_Filters(t *testing.T) {
	store := memory.NewEventStore()
	for i := range 40 {
		// One tocked in "order-" every ten events; the window has to widen
		// to find them.
		tock := i%10 == 3
		stream := "cart-1"
		if tock {
			stream = "order-1"
		}
		appendEvent(t, store, stream, tock, "", "")
	}
	appendEvent(t, store, "cart-2", true, "", "")
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithEvents(store))

	var page eventsPage
	getJSON(t, d, "/api/events?limit=3&type=dashboard_test.Tocked&stream=order-", http.StatusOK, &page)
	if got, want := page.positions(), []uint64{34, 24, 14}; !slices.Equal(got, want) {
		t.Errorf("filtered page = %v, want %v", got, want)
	}
	for _, e := range page.Events {
		if e.Type != "dashboard_test.Tocked" || e.StreamID != "order-1" {
			t.Errorf("event %d is %s in %s, want a tocked in order-1", e.Position, e.Type, e.StreamID)
		}
		if string(e.Payload) != `{"ID":"order-1"}` {
			t.Errorf("payload = %s, want the event as JSON", e.Payload)
		}
	}

	getJSON(t, d, fmt.Sprintf("/api/events?limit=3&type=dashboard_test.Tocked&stream=order-&before=%d", page.Older), http.StatusOK, &page)
	if got, want := page.positions(), []uint64{4}; !slices.Equal(got, want) || page.Older != 0 {
		t.Errorf("older page = %v, older %d, want %v and nothing older", got, page.Older, want)
	}
}

func TestEventList_IsOffWithoutWithEvents(t *testing.T) {
	d := dashboard.New(memory.NewProjectionStore())
	getJSON(t, d, "/api/events", http.StatusNotFound, nil)
	getJSON(t, d, "/api/correlations/x", http.StatusNotFound, nil)
	if s := getSnapshot(t, d); s.Events || s.Chains {
		t.Errorf("snapshot offers events %v, chains %v, want neither", s.Events, s.Chains)
	}
}

func TestChain_LoadsAcrossStreams(t *testing.T) {
	store := memory.NewEventStore()
	root := appendEvent(t, store, "cart-1", false, "chain-1", "")
	appendEvent(t, store, "cart-9", false, "chain-2", "")
	child := appendEvent(t, store, "order-1", true, "chain-1", root)
	grandchild := appendEvent(t, store, "invoice-1", false, "chain-1", child)
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithEvents(store))

	if s := getSnapshot(t, d); !s.Events || !s.Chains {
		t.Errorf("snapshot offers events %v, chains %v, want both", s.Events, s.Chains)
	}
	var page eventsPage
	getJSON(t, d, "/api/correlations/"+url.PathEscape("chain-1"), http.StatusOK, &page)
	var ids, causes []string
	for _, e := range page.Events {
		ids = append(ids, e.EventID)
		causes = append(causes, e.CausationID)
	}
	if want := []string{root, child, grandchild}; !slices.Equal(ids, want) {
		t.Errorf("chain = %v, want %v, oldest first", ids, want)
	}
	if want := []string{"", root, child}; !slices.Equal(causes, want) {
		t.Errorf("causes = %v, want %v", causes, want)
	}

	getJSON(t, d, "/api/correlations/no-such-chain", http.StatusOK, &page)
	if len(page.Events) != 0 {
		t.Errorf("unknown chain returned %d events, want 0", len(page.Events))
	}
}

// globalOnly hides a store's LoadCorrelation, as a store that cannot load
// chains would.
type globalOnly struct{ eventsourcing.GlobalReader }

func TestChain_IsOffWhenTheStoreCannotLoadChains(t *testing.T) {
	d := dashboard.New(memory.NewProjectionStore(), dashboard.WithEvents(globalOnly{memory.NewEventStore()}))
	getJSON(t, d, "/api/events", http.StatusOK, nil)
	getJSON(t, d, "/api/correlations/x", http.StatusNotFound, nil)
	if s := getSnapshot(t, d); !s.Events || s.Chains {
		t.Errorf("snapshot offers events %v, chains %v, want events only", s.Events, s.Chains)
	}
}
