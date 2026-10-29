package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/terraskye/eventsourcing"
)

const (
	// defaultEventPage is how many events the events view shows at once.
	defaultEventPage = 50
	// maxEventPage caps the limit a request may ask for.
	maxEventPage = 500
	// maxEventScan caps how many positions one events request scans looking
	// for matches, so a filter that matches rarely cannot read the whole log.
	maxEventScan = 100_000
)

// eventView is one event as the page shows it.
type eventView struct {
	Position      uint64          `json:"position"`
	EventID       string          `json:"eventId"`
	StreamID      string          `json:"streamId"`
	Version       uint64          `json:"version"`
	Type          string          `json:"type"`
	OccurredAt    time.Time       `json:"occurredAt"`
	CorrelationID string          `json:"correlationId,omitempty"`
	CausationID   string          `json:"causationId,omitempty"`
	Metadata      map[string]any  `json:"metadata,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

func newEventView(env *eventsourcing.Envelope) eventView {
	payload, err := eventsourcing.EncodeEvent(env.Event)
	if err != nil {
		// Shown in place of the payload: the event was read, so the rest of
		// the row is still worth seeing.
		payload, _ = json.Marshal(map[string]string{"error": err.Error()})
	}
	return eventView{
		Position:      env.GlobalPosition,
		EventID:       env.EventID.String(),
		StreamID:      env.StreamID,
		Version:       env.Version,
		Type:          env.Event.EventType(),
		OccurredAt:    env.OccurredAt,
		CorrelationID: env.CorrelationID,
		CausationID:   env.CausationID,
		Metadata:      env.Metadata,
		Payload:       payload,
	}
}

// eventsPage is the answer to GET /api/events.
type eventsPage struct {
	// Events holds the matching events, newest first.
	Events []eventView `json:"events"`
	// Head is the newest position in the log.
	Head uint64 `json:"head"`
	// Older is the before to pass for the next, older page, or 0 when the
	// scan reached the start of the log.
	Older uint64 `json:"older"`
	// Scanned reports how far back the read looked, as the lowest position
	// it scanned, when it stopped at maxEventScan before filling the page.
	Scanned uint64 `json:"scanned,omitempty"`
}

// serveEventList answers with a page of the global log, newest first:
// up to limit events before position before (the head, if not given),
// filtered by event type and stream prefix.
func (d *Dashboard) serveEventList(w http.ResponseWriter, r *http.Request) {
	if d.events == nil {
		writeError(w, http.StatusNotFound, "This dashboard shows no events; see dashboard.WithEvents.")
		return
	}
	q := r.URL.Query()
	limit := defaultEventPage
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit must be a positive number.")
			return
		}
		limit = min(n, maxEventPage)
	}
	var before uint64
	if s := q.Get("before"); s != "" {
		n, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before must be a log position.")
			return
		}
		before = n
	}
	opts := eventsourcing.ReadOptions{EventTypes: q["type"], StreamPrefixes: q["stream"]}

	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	page, err := readBackwards(ctx, d.events, before, limit, opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Reading the log failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// readBackwards reads the newest limit events matching opts before position
// before, or before the end of the log when before is 0. A GlobalReader
// only reads forwards, so it reads a window that ends at before, and
// widens the window until it holds limit matches, reaches the start of the
// log or has scanned maxEventScan positions.
func readBackwards(ctx context.Context, reader eventsourcing.GlobalReader, before uint64, limit int, opts eventsourcing.ReadOptions) (eventsPage, error) {
	head, err := reader.Head(ctx)
	if err != nil {
		return eventsPage{}, err
	}
	page := eventsPage{Head: head, Events: []eventView{}}
	end := head // the last position to read
	if before > 0 && before-1 < head {
		end = before - 1
	}
	if end == 0 {
		return page, nil
	}

	var found []*eventsourcing.Envelope
	window := uint64(limit)
	for {
		start := uint64(0) // exclusive
		if window < end {
			start = end - window
		}
		found, err = readRange(ctx, reader, start, end, opts)
		if err != nil {
			return eventsPage{}, err
		}
		switch {
		case len(found) >= limit:
			found = found[len(found)-limit:]
			page.Older = found[0].GlobalPosition
		case start == 0:
			// The whole log before end is in found; there is nothing older.
		case end-start >= maxEventScan:
			page.Older = start + 1
			page.Scanned = start + 1
		default:
			window = min(window*4, maxEventScan)
			continue
		}
		break
	}

	for i := len(found) - 1; i >= 0; i-- {
		page.Events = append(page.Events, newEventView(found[i]))
	}
	return page, nil
}

// readRange returns the events matching opts with a position in
// (after, end], oldest first.
func readRange(ctx context.Context, reader eventsourcing.GlobalReader, after, end uint64, opts eventsourcing.ReadOptions) ([]*eventsourcing.Envelope, error) {
	var out []*eventsourcing.Envelope
	opts.Limit = maxEventPage
	for after < end {
		res, err := reader.ReadAll(ctx, after, opts)
		if err != nil {
			return nil, err
		}
		for _, env := range res.Events {
			if env.GlobalPosition > end {
				return out, nil
			}
			out = append(out, env)
		}
		if res.AtEnd || res.Next <= after || res.Next >= end {
			break
		}
		after = res.Next
	}
	return out, nil
}

// chainPage is the answer to GET /api/correlations/{id}.
type chainPage struct {
	CorrelationID string `json:"correlationId"`
	// Events holds the chain, oldest first.
	Events []eventView `json:"events"`
}

// serveChain answers with every event of one correlation chain, oldest
// first. The page draws the causation tree from their CausationIDs.
func (d *Dashboard) serveChain(w http.ResponseWriter, r *http.Request) {
	if d.chains == nil {
		writeError(w, http.StatusNotFound, "The event store cannot load correlation chains; it does not implement eventsourcing.CorrelationLoader.")
		return
	}
	id := r.PathValue("id")

	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	it, err := d.chains.LoadCorrelation(ctx, id)
	if errors.Is(err, eventsourcing.ErrEmptyCorrelationID) {
		writeError(w, http.StatusBadRequest, "Give a correlation ID.")
		return
	}
	page := chainPage{CorrelationID: id, Events: []eventView{}}
	if err == nil {
		defer it.Close()
		for it.Next() {
			page.Events = append(page.Events, newEventView(it.Value()))
			if len(page.Events) == maxEventScan {
				break
			}
		}
		err = it.Err()
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Loading the chain failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}
