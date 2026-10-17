package eventsourcingtest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// acceptanceOtherEvent is a second event type for the filter checks. Its
// name extends acceptanceEvent's, so a backend that matches event types by
// prefix instead of exactly returns it for acceptanceEvent's name too.
type acceptanceOtherEvent struct {
	ID string
	N  int
}

func (e *acceptanceOtherEvent) AggregateID() string { return e.ID }
func (e *acceptanceOtherEvent) EventType() string {
	return "eventsourcingtest.acceptanceEventOther"
}

func init() {
	eventsourcing.RegisterEvent(&acceptanceOtherEvent{})
}

type globalAcceptance struct {
	store  eventsourcing.EventStore
	reader eventsourcing.GlobalReader
}

// GlobalReaderAcceptanceTest runs the [eventsourcing.GlobalReader] contract
// against reader as a set of subtests. store must be the store reader reads
// from; the suite appends to it through Save and checks what ReadAll and
// Head return.
//
// Like [AcceptanceTest], it may run against a store that already holds
// events: it only looks at events in streams it created, named with a
// fresh UUID, and reads after the head it found when it started.
//
// The suite checks that:
//
//   - events across several streams come back in strictly ascending
//     GlobalPosition order, with no event returned twice;
//   - ReadAll(after = n) never returns the event at position n, and
//     after = 0 starts at the beginning of the log;
//   - Limit bounds the page, and paging with Next visits every event
//     exactly once;
//   - EventTypes and StreamPrefixes filter exactly, alone and combined,
//     and Next still moves past events that were filtered out;
//   - AtEnd is reported at the head, and when AtEnd is false, Next is
//     greater than after;
//   - Head never exceeds what ReadAll returns;
//   - Save reports, in AppendResult.GlobalPosition, the GlobalPosition
//     ReadAll returns for the last event it appended;
//   - under many concurrent Saves to different streams, some of them
//     multi-event, a reader paging along at the same time sees every event
//     exactly once, so no position is skipped because it became visible
//     late.
//
// The concurrency check is the one that catches stores whose positions can
// become visible out of order, such as SQL sequences assigned before
// commit; run it against the real database, not a mock.
func GlobalReaderAcceptanceTest(t *testing.T, store eventsourcing.EventStore, reader eventsourcing.GlobalReader) {
	t.Helper()

	g := &globalAcceptance{store: store, reader: reader}
	a := &acceptance{store: store}
	a.group(t, "GlobalReader", map[string]func(*testing.T){
		"Order":            g.order,
		"ExclusiveStart":   g.exclusiveStart,
		"LimitAndPaging":   g.limitAndPaging,
		"FilterEventTypes": g.filterEventTypes,
		"FilterPrefixes":   g.filterPrefixes,
		"FilterCombined":   g.filterCombined,
		"AtEnd":            g.atEnd,
		"Head":             g.head,
		"AppendResult":     g.appendResult,
		"ConcurrentSaves":  g.concurrentSaves,
	})
}

func (g *globalAcceptance) order(t *testing.T) {
	start := g.headNow(t)
	first, second, third := newStreamID(), newStreamID(), newStreamID()
	g.save(t, envelopes(first, 1, 2), eventsourcing.NoStream{})
	g.save(t, envelopes(second, 1, 1), eventsourcing.NoStream{})
	g.save(t, envelopes(third, 1, 3), eventsourcing.NoStream{})
	g.save(t, envelopes(first, 3, 1), eventsourcing.Revision(2))

	got := g.readToEnd(t, start, eventsourcing.ReadOptions{Limit: 2}, first, second, third)
	var order []string
	for _, env := range got {
		order = append(order, fmt.Sprintf("%s@%d", label3(env.StreamID, first, second, third), env.Version))
	}
	want := []string{"first@1", "first@2", "second@1", "third@1", "third@2", "third@3", "first@3"}
	if !slices.Equal(order, want) {
		t.Errorf("ReadAll order = %v, want %v", order, want)
	}
}

func (g *globalAcceptance) exclusiveStart(t *testing.T) {
	start := g.headNow(t)
	id := newStreamID()
	g.save(t, envelopes(id, 1, 3), eventsourcing.NoStream{})

	all := g.readToEnd(t, start, eventsourcing.ReadOptions{}, id)
	if len(all) != 3 {
		t.Fatalf("ReadAll returned %d of this test's events, want 3", len(all))
	}
	checkpoint := all[0].GlobalPosition
	got := g.readToEnd(t, checkpoint, eventsourcing.ReadOptions{}, id)
	if vs := versions(got); !slices.Equal(vs, []uint64{2, 3}) {
		t.Errorf("ReadAll(after %d) versions = %v, want [2 3]", checkpoint, vs)
	}

	res, err := g.reader.ReadAll(t.Context(), 0, eventsourcing.ReadOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ReadAll(after 0): %v", err)
	}
	if len(res.Events) != 1 {
		t.Fatalf("ReadAll(after 0, Limit 1) on a log with events returned %d events, want 1", len(res.Events))
	}
	if gv := res.Events[0].GlobalPosition; gv == 0 || gv > all[0].GlobalPosition {
		t.Errorf("ReadAll(after 0) starts at GlobalPosition %d, want the first event of the log (0 < gv <= %d)", gv, all[0].GlobalPosition)
	}
}

func (g *globalAcceptance) limitAndPaging(t *testing.T) {
	start := g.headNow(t)
	id := newStreamID()
	g.save(t, envelopes(id, 1, 5), eventsourcing.NoStream{})

	for _, limit := range []int{1, 2, 3} {
		got := g.readToEnd(t, start, eventsourcing.ReadOptions{Limit: limit}, id)
		if vs := versions(got); !slices.Equal(vs, []uint64{1, 2, 3, 4, 5}) {
			t.Errorf("paging with Limit %d visited versions %v, want [1 2 3 4 5]", limit, vs)
		}
	}
}

// saveMixed saves, to two fresh streams with distinct prefixes, events of
// both acceptance types and returns the two stream IDs (whose prefixes are
// the IDs minus their last character) and the GlobalPosition of the last
// event saved.
func (g *globalAcceptance) saveMixed(t *testing.T) (a, b string, last uint64) {
	t.Helper()
	start := g.headNow(t)
	tag := uuid.NewString()
	a, b = "acceptance-a-"+tag, "acceptance-b-"+tag
	g.save(t, envelopes(a, 1, 2), eventsourcing.NoStream{})
	g.save(t, otherEnvelopes(b, 1, 2), eventsourcing.NoStream{})
	g.save(t, otherEnvelopes(a, 3, 1), eventsourcing.Revision(2))
	g.save(t, envelopes(b, 3, 1), eventsourcing.Revision(2))
	all := g.readToEnd(t, start, eventsourcing.ReadOptions{}, a, b)
	if len(all) != 6 {
		t.Fatalf("unfiltered ReadAll returned %d of this test's events, want 6", len(all))
	}
	return a, b, all[len(all)-1].GlobalPosition
}

func (g *globalAcceptance) filterEventTypes(t *testing.T) {
	start := g.headNow(t)
	a, b, last := g.saveMixed(t)

	opts := eventsourcing.ReadOptions{EventTypes: []string{(&acceptanceEvent{}).EventType()}, Limit: 2}
	got, next := g.readToEndNext(t, start, opts, a, b)
	if l := streamVersions(got, a, b); !slices.Equal(l, []string{"a@1", "a@2", "b@3"}) {
		t.Errorf("EventTypes filter returned %v, want [a@1 a@2 b@3] (exact match, not prefix)", l)
	}
	if next < last {
		t.Errorf("filtered read ended with Next = %d, want at least %d: Next must move past filtered-out events", next, last)
	}
}

func (g *globalAcceptance) filterPrefixes(t *testing.T) {
	start := g.headNow(t)
	a, b, last := g.saveMixed(t)

	opts := eventsourcing.ReadOptions{StreamPrefixes: []string{"no-such-prefix-", a[:len(a)-1]}, Limit: 2}
	got, next := g.readToEndNext(t, start, opts, a, b)
	if l := streamVersions(got, a, b); !slices.Equal(l, []string{"a@1", "a@2", "a@3"}) {
		t.Errorf("StreamPrefixes filter returned %v, want [a@1 a@2 a@3]", l)
	}
	if next < last {
		t.Errorf("filtered read ended with Next = %d, want at least %d", next, last)
	}
	// Characters that are wildcards in SQL LIKE must match literally.
	res, err := g.reader.ReadAll(t.Context(), start, eventsourcing.ReadOptions{StreamPrefixes: []string{"acceptance-_-%"}})
	if err != nil {
		t.Fatalf("ReadAll with a wildcard-looking prefix: %v", err)
	}
	if n := len(filter(res.Events, a, b)); n != 0 {
		t.Errorf("prefix %q matched %d of this test's events, want 0: prefixes must match literally", "acceptance-_-%", n)
	}
}

func (g *globalAcceptance) filterCombined(t *testing.T) {
	start := g.headNow(t)
	a, b, _ := g.saveMixed(t)

	opts := eventsourcing.ReadOptions{
		EventTypes:     []string{(&acceptanceOtherEvent{}).EventType()},
		StreamPrefixes: []string{b[:len(b)-1]},
	}
	got := g.readToEnd(t, start, opts, a, b)
	if l := streamVersions(got, a, b); !slices.Equal(l, []string{"b@1", "b@2"}) {
		t.Errorf("combined filter returned %v, want [b@1 b@2]", l)
	}
}

func (g *globalAcceptance) atEnd(t *testing.T) {
	start := g.headNow(t)
	g.save(t, envelopes(newStreamID(), 1, 2), eventsourcing.NoStream{})

	_, next := g.readToEndNext(t, start, eventsourcing.ReadOptions{Limit: 1})
	res, err := g.reader.ReadAll(t.Context(), next, eventsourcing.ReadOptions{})
	if err != nil {
		t.Fatalf("ReadAll at the head: %v", err)
	}
	// Other tests may share the store and append in between; only an
	// empty page has to be at the end.
	if len(res.Events) == 0 && !res.AtEnd {
		t.Errorf("ReadAll(after %d) at the head = no events, AtEnd false, want AtEnd true", next)
	}
	if len(res.Events) == 0 && res.Next != next {
		t.Errorf("ReadAll(after %d) at the head = Next %d, want %d (unchanged when nothing was scanned)", next, res.Next, next)
	}
}

func (g *globalAcceptance) head(t *testing.T) {
	start := g.headNow(t)
	id := newStreamID()
	g.save(t, envelopes(id, 1, 2), eventsourcing.NoStream{})

	h := g.headNow(t)
	got, next := g.readToEndNext(t, start, eventsourcing.ReadOptions{}, id)
	if len(got) != 2 {
		t.Fatalf("ReadAll returned %d of this test's events, want 2", len(got))
	}
	if h < got[1].GlobalPosition {
		t.Errorf("Head() = %d after saving an event at %d, want at least %d", h, got[1].GlobalPosition, got[1].GlobalPosition)
	}
	if next < h {
		t.Errorf("reading to the end stopped at Next = %d, below Head %d: Head must not exceed what ReadAll returns", next, h)
	}
}

func (g *globalAcceptance) concurrentSaves(t *testing.T) {
	const writers, savesPerWriter = 12, 15
	start := g.headNow(t)

	ids := make([]string, writers)
	for i := range ids {
		ids[i] = newStreamID()
	}
	want := 0
	for w := range writers {
		for s := range savesPerWriter {
			want += 1 + (w+s)%3
		}
	}

	var wg sync.WaitGroup
	done := make(chan struct{})
	// Wait for the writers on every return path, so none outlives the test.
	defer func() { <-done }()
	errs := make(chan error, writers)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var version uint64
			for s := range savesPerWriter {
				n := 1 + (w+s)%3
				if _, err := g.store.Save(context.Background(), envelopes(ids[w], version+1, n), eventsourcing.Revision(version)); err != nil {
					errs <- fmt.Errorf("writer %d save %d: %w", w, s, err)
					return
				}
				version += uint64(n)
			}
		}()
	}
	go func() { wg.Wait(); close(done) }()

	seen := make(map[string]uint64) // stream -> last version seen
	count := 0
	after := start
	writing := true
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if time.Now().After(deadline) {
			t.Errorf("reader did not finish within 2 minutes; saw %d of %d events", count, want)
			return
		}
		res, err := g.reader.ReadAll(t.Context(), after, eventsourcing.ReadOptions{Limit: 7})
		if err != nil {
			t.Fatalf("ReadAll(after %d): %v", after, err)
		}
		if err := checkPage(after, res); err != nil {
			t.Error(err)
			return
		}
		for _, env := range res.Events {
			if !slices.Contains(ids, env.StreamID) {
				continue
			}
			if expected := seen[env.StreamID] + 1; env.Version != expected {
				t.Errorf("stream %q: saw version %d after %d, want %d; an event was skipped or repeated (GlobalPosition %d)",
					env.StreamID, env.Version, seen[env.StreamID], expected, env.GlobalPosition)
				return
			}
			seen[env.StreamID] = env.Version
			count++
		}
		after = res.Next
		if res.AtEnd {
			if !writing {
				break
			}
			select {
			case <-done:
				writing = false // one more pass after the last Save
			default:
				time.Sleep(time.Millisecond)
			}
		}
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if count != want {
		t.Errorf("reader saw %d events, want %d", count, want)
	}
}

func (g *globalAcceptance) appendResult(t *testing.T) {
	start := g.headNow(t)
	id := newStreamID()
	res, err := g.store.Save(t.Context(), envelopes(id, 1, 3), eventsourcing.NoStream{})
	if err != nil {
		t.Fatalf("Save(NoStream) to %q: %v", id, err)
	}
	got := g.readToEnd(t, start, eventsourcing.ReadOptions{}, id)
	if len(got) != 3 {
		t.Fatalf("ReadAll returned %d of this test's events, want 3", len(got))
	}
	if want := got[2].GlobalPosition; res.GlobalPosition != want {
		t.Errorf("AppendResult.GlobalPosition = %d, want %d, the GlobalPosition of the last event appended", res.GlobalPosition, want)
	}
}

// --- helpers ---

func (g *globalAcceptance) headNow(t *testing.T) uint64 {
	t.Helper()
	h, err := g.reader.Head(t.Context())
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	return h
}

func (g *globalAcceptance) save(t *testing.T, events []eventsourcing.Envelope, state eventsourcing.StreamState) {
	t.Helper()
	if _, err := g.store.Save(t.Context(), events, state); err != nil {
		t.Fatalf("Save(%v) to %q: %v", state, events[0].StreamID, err)
	}
}

func (g *globalAcceptance) readToEnd(t *testing.T, after uint64, opts eventsourcing.ReadOptions, ids ...string) []*eventsourcing.Envelope {
	t.Helper()
	got, _ := g.readToEndNext(t, after, opts, ids...)
	return got
}

// readToEndNext pages through the log from after until AtEnd, checking
// every page against the contract, and returns the events of ids (all
// events when ids is empty) and the final Next. A page that breaks the
// contract is reported with t.Error and ends the read early, returning what
// was read so far.
func (g *globalAcceptance) readToEndNext(t *testing.T, after uint64, opts eventsourcing.ReadOptions, ids ...string) ([]*eventsourcing.Envelope, uint64) {
	t.Helper()
	var out []*eventsourcing.Envelope
	for range 100_000 {
		res, err := g.reader.ReadAll(t.Context(), after, opts)
		if err != nil {
			t.Fatalf("ReadAll(after %d, %+v): %v", after, opts, err)
		}
		if err := checkPage(after, res); err != nil {
			t.Error(err)
			return out, after
		}
		if opts.Limit > 0 && len(res.Events) > opts.Limit {
			t.Errorf("ReadAll(after %d, Limit %d) returned %d events, want at most %d", after, opts.Limit, len(res.Events), opts.Limit)
		}
		for _, env := range res.Events {
			if len(opts.EventTypes) > 0 && !slices.Contains(opts.EventTypes, env.Event.EventType()) {
				t.Errorf("ReadAll(EventTypes %v) returned an event of type %q", opts.EventTypes, env.Event.EventType())
			}
			if len(opts.StreamPrefixes) > 0 && !slices.ContainsFunc(opts.StreamPrefixes, func(p string) bool {
				return len(env.StreamID) >= len(p) && env.StreamID[:len(p)] == p
			}) {
				t.Errorf("ReadAll(StreamPrefixes %v) returned an event of stream %q", opts.StreamPrefixes, env.StreamID)
			}
		}
		if len(ids) == 0 {
			out = append(out, res.Events...)
		} else {
			out = append(out, filter(res.Events, ids...)...)
		}
		after = res.Next
		if res.AtEnd {
			return out, after
		}
	}
	t.Errorf("ReadAll did not reach AtEnd")
	return out, after
}

// checkPage checks one ReadAll(after) result against the contract and
// returns an error describing the first violation, or nil.
func checkPage(after uint64, res eventsourcing.ReadResult) error {
	prev := after
	for _, env := range res.Events {
		if env.GlobalPosition <= prev {
			return fmt.Errorf("ReadAll(after %d) returned GlobalPosition %d after %d, want strictly ascending and exclusive of after",
				after, env.GlobalPosition, prev)
		}
		prev = env.GlobalPosition
	}
	if len(res.Events) > 0 && res.Next < prev {
		return fmt.Errorf("ReadAll(after %d).Next = %d, want at least its last event's GlobalPosition %d", after, res.Next, prev)
	}
	if res.Next < after {
		return fmt.Errorf("ReadAll(after %d).Next = %d, want at least %d", after, res.Next, after)
	}
	if !res.AtEnd && res.Next <= after {
		return fmt.Errorf("ReadAll(after %d) = AtEnd false, Next %d, want Next > %d when not at the end", after, res.Next, after)
	}
	return nil
}

func otherEnvelopes(id string, from uint64, count int) []eventsourcing.Envelope {
	out := envelopes(id, from, count)
	for i := range out {
		v := from + uint64(i)
		out[i].Event = &acceptanceOtherEvent{ID: id, N: int(v)}
	}
	return out
}

func filter(envs []*eventsourcing.Envelope, ids ...string) []*eventsourcing.Envelope {
	var out []*eventsourcing.Envelope
	for _, env := range envs {
		if slices.Contains(ids, env.StreamID) {
			out = append(out, env)
		}
	}
	return out
}

func streamVersions(envs []*eventsourcing.Envelope, a, b string) []string {
	var out []string
	for _, env := range envs {
		out = append(out, fmt.Sprintf("%s@%d", label3(env.StreamID, a, b, ""), env.Version))
	}
	return out
}

func label3(id, first, second, third string) string {
	switch id {
	case first:
		if third == "" {
			return "a"
		}
		return "first"
	case second:
		if third == "" {
			return "b"
		}
		return "second"
	case third:
		return "third"
	}
	return id
}
