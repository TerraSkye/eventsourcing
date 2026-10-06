// Package eventsourcingtest provides [AcceptanceTest], the contract every
// [eventsourcing.EventStore] implementation in this module is checked
// against. Each implementation, and any third-party store, runs the same
// suite from its own tests:
//
//	func TestAcceptance(t *testing.T) {
//		eventsourcingtest.AcceptanceTest(t, memory.NewMemoryStore(100))
//	}
package eventsourcingtest

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// acceptanceEvent is the only event type the suite saves. Its name is
// namespaced so registering it cannot collide with a caller's own events.
type acceptanceEvent struct {
	ID string
	N  int
}

func (e *acceptanceEvent) AggregateID() string { return e.ID }
func (e *acceptanceEvent) EventType() string   { return "eventsourcingtest.acceptanceEvent" }

func init() {
	eventsourcing.RegisterEvent(&acceptanceEvent{})
}

type acceptance struct {
	store eventsourcing.EventStore
}

// AcceptanceTest runs the [eventsourcing.EventStore] contract against store
// as a set of subtests.
//
// store may already hold events, and may be shared with other tests: every
// subtest writes to streams of its own, named with a fresh UUID, and reads
// from [eventsourcing.EventStore.LoadFromAll] only the events of those
// streams. The suite never calls Close; that is left to the caller, who
// owns store.
//
// Envelopes are saved with Version already set, 1 for the first event of a
// stream and counting up, the way [eventsourcing.NewCommandHandler] fills
// them in, so a store that relies on the caller for Version and one that
// assigns it itself are held to the same result.
func AcceptanceTest(t *testing.T, store eventsourcing.EventStore) {
	t.Helper()

	a := &acceptance{store: store}

	a.group(t, "Save", map[string]func(*testing.T){
		"EmptyBatch":                a.saveEmptyBatch,
		"NoStreamCreatesStream":     a.saveNoStreamCreatesStream,
		"NoStreamRejectsExisting":   a.saveNoStreamRejectsExisting,
		"StreamExistsRejectsNew":    a.saveStreamExistsRejectsNew,
		"StreamExistsAppends":       a.saveStreamExistsAppends,
		"AnyAppends":                a.saveAnyAppends,
		"RevisionAppends":           a.saveRevisionAppends,
		"RevisionConflict":          a.saveRevisionConflict,
		"MixedStreamsRejected":      a.saveMixedStreamsRejected,
		"ConcurrentRevisionOneWins": a.saveConcurrentRevisionOneWins,
	})
	a.group(t, "LoadStream", map[string]func(*testing.T){
		"MissingStream": a.loadStreamMissing,
		"Order":         a.loadStreamOrder,
		"Versions":      a.loadStreamVersions,
		"RoundTrip":     a.loadStreamRoundTrip,
		"CloseEarly":    a.loadStreamCloseEarly,
	})
	a.group(t, "LoadStreamFrom", map[string]func(*testing.T){
		"AnyReadsWholeStream":     a.loadFromAnyReadsWholeStream,
		"AnyOnMissingStreamEmpty": a.loadFromAnyOnMissingStream,
		"RevisionIsExclusive":     a.loadFromRevisionIsExclusive,
		"ResumeFromLastVersion":   a.loadFromResumeFromLastVersion,
		"StreamExistsRejectsNew":  a.loadFromStreamExistsRejectsNew,
		"NoStreamRejectsExisting": a.loadFromNoStreamRejectsExisting,
	})
	a.group(t, "LoadFromAll", map[string]func(*testing.T){
		"IncludesSavedEvents":   a.loadAllIncludesSavedEvents,
		"GlobalVersionIncrease": a.loadAllGlobalVersionIncreases,
		"RevisionIsExclusive":   a.loadAllRevisionIsExclusive,
	})
}

// group runs tests as subtests of a subtest named name, in a stable order.
func (a *acceptance) group(t *testing.T, name string, tests map[string]func(*testing.T)) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		names := make([]string, 0, len(tests))
		for n := range tests {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			t.Run(n, tests[n])
		}
	})
}

// --- Save ---

func (a *acceptance) saveEmptyBatch(t *testing.T) {
	res, err := a.store.Save(t.Context(), nil, eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	if !res.Successful {
		t.Errorf("Save(nil).Successful = false, want true")
	}
}

func (a *acceptance) saveNoStreamCreatesStream(t *testing.T) {
	id := newStreamID()
	res, err := a.store.Save(t.Context(), envelopes(id, 1, 3), eventsourcing.NoStream{})
	if err != nil {
		t.Fatalf("Save(eventsourcing.NoStream) on a new stream: %v", err)
	}
	if !res.Successful {
		t.Errorf("Successful = false, want true")
	}
	if res.StreamID != id {
		t.Errorf("StreamID = %q, want %q", res.StreamID, id)
	}
	if res.NextExpectedVersion != 3 {
		t.Errorf("NextExpectedVersion = %d, want 3", res.NextExpectedVersion)
	}
	a.wantStreamLen(t, id, 3)
}

func (a *acceptance) saveNoStreamRejectsExisting(t *testing.T) {
	id := a.seed(t, 2)
	_, err := a.store.Save(t.Context(), envelopes(id, 3, 1), eventsourcing.NoStream{})
	if !errors.Is(err, eventsourcing.ErrStreamExists) {
		t.Errorf("Save(eventsourcing.NoStream) on an existing stream: err = %v, want %v", err, eventsourcing.ErrStreamExists)
	}
	a.wantStreamLen(t, id, 2)
}

func (a *acceptance) saveStreamExistsRejectsNew(t *testing.T) {
	id := newStreamID()
	_, err := a.store.Save(t.Context(), envelopes(id, 1, 1), eventsourcing.StreamExists{})
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("Save(eventsourcing.StreamExists) on a new stream: err = %v, want %v", err, eventsourcing.ErrStreamNotFound)
	}
	a.wantStreamLen(t, id, 0)
}

func (a *acceptance) saveStreamExistsAppends(t *testing.T) {
	id := a.seed(t, 2)
	res, err := a.store.Save(t.Context(), envelopes(id, 3, 1), eventsourcing.StreamExists{})
	if err != nil {
		t.Fatalf("Save(eventsourcing.StreamExists) on an existing stream: %v", err)
	}
	if res.NextExpectedVersion != 3 {
		t.Errorf("NextExpectedVersion = %d, want 3", res.NextExpectedVersion)
	}
	a.wantStreamLen(t, id, 3)
}

func (a *acceptance) saveAnyAppends(t *testing.T) {
	id := newStreamID()
	if _, err := a.store.Save(t.Context(), envelopes(id, 1, 2), eventsourcing.Any{}); err != nil {
		t.Fatalf("Save(eventsourcing.Any) on a new stream: %v", err)
	}
	res, err := a.store.Save(t.Context(), envelopes(id, 3, 2), eventsourcing.Any{})
	if err != nil {
		t.Fatalf("Save(eventsourcing.Any) on an existing stream: %v", err)
	}
	if res.NextExpectedVersion != 4 {
		t.Errorf("NextExpectedVersion = %d, want 4", res.NextExpectedVersion)
	}
	a.wantStreamLen(t, id, 4)
}

// saveRevisionAppends checks Revision(N) against a stream of N events, the
// expectation NewCommandHandler saves with after folding N events —
// including Revision(0) for a stream that does not exist yet.
func (a *acceptance) saveRevisionAppends(t *testing.T) {
	id := newStreamID()
	res, err := a.store.Save(t.Context(), envelopes(id, 1, 2), eventsourcing.Revision(0))
	if err != nil {
		t.Fatalf("Save(eventsourcing.Revision(0)) on a new stream: %v", err)
	}
	if res.NextExpectedVersion != 2 {
		t.Errorf("NextExpectedVersion = %d, want 2", res.NextExpectedVersion)
	}
	res, err = a.store.Save(t.Context(), envelopes(id, 3, 1), eventsourcing.Revision(res.NextExpectedVersion))
	if err != nil {
		t.Fatalf("Save(eventsourcing.Revision(NextExpectedVersion)): %v", err)
	}
	if res.NextExpectedVersion != 3 {
		t.Errorf("NextExpectedVersion = %d, want 3", res.NextExpectedVersion)
	}
	a.wantStreamLen(t, id, 3)
}

func (a *acceptance) saveRevisionConflict(t *testing.T) {
	id := a.seed(t, 2)
	for _, rev := range []eventsourcing.Revision{0, 1, 3} {
		_, err := a.store.Save(t.Context(), envelopes(id, 3, 1), rev)
		var conflict *eventsourcing.StreamRevisionConflictError
		if !errors.As(err, &conflict) {
			t.Errorf("Save(eventsourcing.Revision(%d)) on a stream of 2: err = %v, want a %T", rev, err, conflict)
			continue
		}
		if conflict.Stream != id {
			t.Errorf("Save(eventsourcing.Revision(%d)): conflict.Stream = %q, want %q", rev, conflict.Stream, id)
		}
	}
	a.wantStreamLen(t, id, 2)
}

func (a *acceptance) saveMixedStreamsRejected(t *testing.T) {
	first, second := newStreamID(), newStreamID()
	batch := append(envelopes(first, 1, 1), envelopes(second, 1, 1)...)
	_, err := a.store.Save(t.Context(), batch, eventsourcing.Any{})
	if !errors.Is(err, eventsourcing.ErrInvalidEventBatch) {
		t.Errorf("Save of a batch spanning two streams: err = %v, want %v", err, eventsourcing.ErrInvalidEventBatch)
	}
	a.wantStreamLen(t, first, 0)
	a.wantStreamLen(t, second, 0)
}

// saveConcurrentRevisionOneWins races several writers that all loaded the
// same stream, which is what optimistic concurrency exists to arbitrate:
// exactly one append lands and every other writer is told to reload.
func (a *acceptance) saveConcurrentRevisionOneWins(t *testing.T) {
	const writers = 8
	id := a.seed(t, 1)

	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Go(func() {
			_, errs[i] = a.store.Save(t.Context(), envelopes(id, 2, 1), eventsourcing.Revision(1))
		})
	}
	wg.Wait()

	var won int
	for i, err := range errs {
		var conflict *eventsourcing.StreamRevisionConflictError
		switch {
		case err == nil:
			won++
		case !errors.As(err, &conflict):
			t.Errorf("writer %d: err = %v, want nil or a %T", i, err, conflict)
		}
	}
	if won != 1 {
		t.Errorf("%d of %d concurrent Save(eventsourcing.Revision(1)) calls succeeded, want exactly 1", won, writers)
	}
	a.wantStreamLen(t, id, 2)
}

// --- LoadStream ---

// loadStreamMissing accepts ErrStreamNotFound from LoadStream itself or from
// the iterator it returns: a store that reads lazily only learns the stream
// is missing once the first event is requested.
func (a *acceptance) loadStreamMissing(t *testing.T) {
	it, err := a.store.LoadStream(t.Context(), newStreamID())
	if err == nil {
		defer it.Close()
		for it.Next() {
			t.Fatalf("LoadStream of a missing stream yielded an event: %+v", it.Value())
		}
		err = it.Err()
	}
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("LoadStream of a missing stream: err = %v, want %v", err, eventsourcing.ErrStreamNotFound)
	}
}

func (a *acceptance) loadStreamOrder(t *testing.T) {
	id := newStreamID()
	a.save(t, envelopes(id, 1, 2), eventsourcing.NoStream{})
	a.save(t, envelopes(id, 3, 3), eventsourcing.Revision(2))

	got := a.loadStream(t, id)
	if ns := payloadNs(t, got); !slices.Equal(ns, []int{1, 2, 3, 4, 5}) {
		t.Errorf("LoadStream order = %v, want [1 2 3 4 5]", ns)
	}
}

func (a *acceptance) loadStreamVersions(t *testing.T) {
	id := a.seed(t, 3)
	got := a.loadStream(t, id)
	if vs := versions(got); !slices.Equal(vs, []uint64{1, 2, 3}) {
		t.Errorf("LoadStream versions = %v, want [1 2 3] (eventsourcing.Envelope.Version starts at 1)", vs)
	}
}

func (a *acceptance) loadStreamRoundTrip(t *testing.T) {
	id := newStreamID()
	in := envelopes(id, 1, 1)
	a.save(t, in, eventsourcing.NoStream{})

	got := a.loadStream(t, id)
	if len(got) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(got))
	}
	out := got[0]
	if out.EventID != in[0].EventID {
		t.Errorf("EventID = %v, want %v", out.EventID, in[0].EventID)
	}
	if out.StreamID != id {
		t.Errorf("StreamID = %q, want %q", out.StreamID, id)
	}
	ev, ok := out.Event.(*acceptanceEvent)
	if !ok {
		t.Fatalf("Event = %T, want %T", out.Event, ev)
	}
	if want := in[0].Event.(*acceptanceEvent); *ev != *want {
		t.Errorf("Event = %+v, want %+v", *ev, *want)
	}
	if got, want := out.Metadata["acceptance"], in[0].Metadata["acceptance"]; got != want {
		t.Errorf(`Metadata["acceptance"] = %v, want %v`, got, want)
	}
	if out.OccurredAt.IsZero() {
		t.Errorf("OccurredAt is zero")
	}
}

// loadStreamCloseEarly stops reading after the first event, as a caller
// that finds what it needs does, and checks the store is still usable.
func (a *acceptance) loadStreamCloseEarly(t *testing.T) {
	id := a.seed(t, 3)
	it, err := a.store.LoadStream(t.Context(), id)
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	if !it.Next() {
		t.Fatalf("LoadStream yielded no events: %v", it.Err())
	}
	if err := it.Close(); err != nil {
		t.Errorf("Close after one event: %v", err)
	}
	if it.Next() {
		t.Errorf("Next after Close returned true")
	}
	a.save(t, envelopes(id, 4, 1), eventsourcing.Revision(3))
	a.wantStreamLen(t, id, 4)
}

// --- LoadStreamFrom ---

func (a *acceptance) loadFromAnyReadsWholeStream(t *testing.T) {
	id := a.seed(t, 3)
	got := a.loadStreamFrom(t, id, eventsourcing.Any{})
	if vs := versions(got); !slices.Equal(vs, []uint64{1, 2, 3}) {
		t.Errorf("LoadStreamFrom(eventsourcing.Any) versions = %v, want [1 2 3]", vs)
	}
}

// loadFromAnyOnMissingStream is how NewCommandHandler loads an aggregate
// that has no events yet: an empty read, not an error.
func (a *acceptance) loadFromAnyOnMissingStream(t *testing.T) {
	got := a.loadStreamFrom(t, newStreamID(), eventsourcing.Any{})
	if len(got) != 0 {
		t.Errorf("LoadStreamFrom(eventsourcing.Any) of a missing stream returned %d events, want 0", len(got))
	}
}

// loadFromRevisionIsExclusive pins Revision(N) to "N events already
// consumed": only versions above N come back.
func (a *acceptance) loadFromRevisionIsExclusive(t *testing.T) {
	id := a.seed(t, 3)
	for n := range uint64(4) {
		got := a.loadStreamFrom(t, id, eventsourcing.Revision(n))
		var want []uint64
		for v := n + 1; v <= 3; v++ {
			want = append(want, v)
		}
		if vs := versions(got); !slices.Equal(vs, want) {
			t.Errorf("LoadStreamFrom(eventsourcing.Revision(%d)) versions = %v, want %v", n, vs, want)
		}
	}
}

// loadFromResumeFromLastVersion replays NewCommandHandler's retry after a
// conflict: resume from the Version of the last event folded, and see only
// what was appended since.
func (a *acceptance) loadFromResumeFromLastVersion(t *testing.T) {
	id := a.seed(t, 2)
	first := a.loadStreamFrom(t, id, eventsourcing.Any{})
	if len(first) != 2 {
		t.Fatalf("LoadStreamFrom(eventsourcing.Any) returned %d events, want 2", len(first))
	}
	last := eventsourcing.Revision(first[len(first)-1].Version)

	if got := a.loadStreamFrom(t, id, last); len(got) != 0 {
		t.Errorf("LoadStreamFrom(eventsourcing.Revision(%d)) before any append returned versions %v, want none", last, versions(got))
	}

	a.save(t, envelopes(id, 3, 1), eventsourcing.Any{})
	got := a.loadStreamFrom(t, id, last)
	if ns := payloadNs(t, got); !slices.Equal(ns, []int{3}) {
		t.Errorf("LoadStreamFrom(eventsourcing.Revision(%d)) after one append returned events %v, want [3]", last, ns)
	}
}

func (a *acceptance) loadFromStreamExistsRejectsNew(t *testing.T) {
	it, err := a.store.LoadStreamFrom(t.Context(), newStreamID(), eventsourcing.StreamExists{})
	if err == nil {
		defer it.Close()
		for it.Next() {
		}
		err = it.Err()
	}
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("LoadStreamFrom(eventsourcing.StreamExists) of a missing stream: err = %v, want %v", err, eventsourcing.ErrStreamNotFound)
	}
}

func (a *acceptance) loadFromNoStreamRejectsExisting(t *testing.T) {
	id := a.seed(t, 1)
	it, err := a.store.LoadStreamFrom(t.Context(), id, eventsourcing.NoStream{})
	if err == nil {
		defer it.Close()
		for it.Next() {
		}
		err = it.Err()
	}
	if !errors.Is(err, eventsourcing.ErrStreamExists) {
		t.Errorf("LoadStreamFrom(eventsourcing.NoStream) of an existing stream: err = %v, want %v", err, eventsourcing.ErrStreamExists)
	}
}

// --- LoadFromAll ---

func (a *acceptance) loadAllIncludesSavedEvents(t *testing.T) {
	first, second := newStreamID(), newStreamID()
	a.save(t, envelopes(first, 1, 1), eventsourcing.NoStream{})
	a.save(t, envelopes(second, 1, 1), eventsourcing.NoStream{})
	a.save(t, envelopes(first, 2, 1), eventsourcing.Revision(1))

	got := a.loadAll(t, eventsourcing.Any{}, first, second)
	var order []string
	for _, env := range got {
		order = append(order, fmt.Sprintf("%s@%d", label(env.StreamID, first, second), env.Version))
	}
	if want := []string{"first@1", "second@1", "first@2"}; !slices.Equal(order, want) {
		t.Errorf("LoadFromAll order = %v, want %v", order, want)
	}
}

func (a *acceptance) loadAllGlobalVersionIncreases(t *testing.T) {
	first, second := newStreamID(), newStreamID()
	a.save(t, envelopes(first, 1, 2), eventsourcing.NoStream{})
	a.save(t, envelopes(second, 1, 2), eventsourcing.NoStream{})

	got := a.loadAll(t, eventsourcing.Any{}, first, second)
	if len(got) != 4 {
		t.Fatalf("LoadFromAll returned %d of this test's events, want 4", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].GlobalVersion <= got[i-1].GlobalVersion {
			t.Errorf("GlobalVersion does not increase: event %d has %d, event %d has %d",
				i-1, got[i-1].GlobalVersion, i, got[i].GlobalVersion)
		}
	}
}

// loadAllRevisionIsExclusive resumes a global read from the GlobalVersion
// of the last event seen, as a projection restarting from its checkpoint
// does.
func (a *acceptance) loadAllRevisionIsExclusive(t *testing.T) {
	id := newStreamID()
	a.save(t, envelopes(id, 1, 3), eventsourcing.NoStream{})

	all := a.loadAll(t, eventsourcing.Any{}, id)
	if len(all) != 3 {
		t.Fatalf("LoadFromAll returned %d of this test's events, want 3", len(all))
	}
	checkpoint := eventsourcing.Revision(all[0].GlobalVersion)

	got := a.loadAll(t, checkpoint, id)
	if vs := versions(got); !slices.Equal(vs, []uint64{2, 3}) {
		t.Errorf("LoadFromAll(eventsourcing.Revision(%d)) versions = %v, want [2 3]", checkpoint, vs)
	}
}

// --- helpers ---

// newStreamID returns a stream ID no other test, or earlier run against a
// persistent store, has used.
func newStreamID() string {
	return "acceptance-" + uuid.NewString()
}

// envelopes returns count envelopes for stream id, with Versions starting at
// from and payloads numbered to match, so a loaded event can be traced back
// to the Save it came from.
func envelopes(id string, from uint64, count int) []eventsourcing.Envelope {
	out := make([]eventsourcing.Envelope, count)
	for i := range out {
		v := from + uint64(i)
		out[i] = eventsourcing.Envelope{
			EventID:    uuid.New(),
			StreamID:   id,
			Event:      &acceptanceEvent{ID: id, N: int(v)},
			Metadata:   map[string]any{"acceptance": "yes"},
			Version:    v,
			OccurredAt: time.Now(),
		}
	}
	return out
}

// seed creates a new stream holding count events and returns its ID.
func (a *acceptance) seed(t *testing.T, count int) string {
	t.Helper()
	id := newStreamID()
	a.save(t, envelopes(id, 1, count), eventsourcing.NoStream{})
	return id
}

func (a *acceptance) save(t *testing.T, events []eventsourcing.Envelope, state eventsourcing.StreamState) {
	t.Helper()
	if _, err := a.store.Save(t.Context(), events, state); err != nil {
		t.Fatalf("Save(%v) to %q: %v", state, events[0].StreamID, err)
	}
}

func (a *acceptance) loadStream(t *testing.T, id string) []*eventsourcing.Envelope {
	t.Helper()
	it, err := a.store.LoadStream(t.Context(), id)
	if err != nil {
		t.Fatalf("LoadStream(%q): %v", id, err)
	}
	return drain(t, it)
}

func (a *acceptance) loadStreamFrom(t *testing.T, id string, state eventsourcing.StreamState) []*eventsourcing.Envelope {
	t.Helper()
	it, err := a.store.LoadStreamFrom(t.Context(), id, state)
	if err != nil {
		t.Fatalf("LoadStreamFrom(%q, %v): %v", id, state, err)
	}
	return drain(t, it)
}

// loadAll reads LoadFromAll from state and keeps only the events of ids, so
// events other tests saved to a shared store do not interfere.
func (a *acceptance) loadAll(t *testing.T, state eventsourcing.StreamState, ids ...string) []*eventsourcing.Envelope {
	t.Helper()
	it, err := a.store.LoadFromAll(t.Context(), state)
	if err != nil {
		t.Fatalf("LoadFromAll(%v): %v", state, err)
	}
	var out []*eventsourcing.Envelope
	for _, env := range drain(t, it) {
		if slices.Contains(ids, env.StreamID) {
			out = append(out, env)
		}
	}
	return out
}

// wantStreamLen checks id holds exactly n events, reading with Any so a
// missing stream counts as zero rather than failing.
func (a *acceptance) wantStreamLen(t *testing.T, id string, n int) {
	t.Helper()
	if got := a.loadStreamFrom(t, id, eventsourcing.Any{}); len(got) != n {
		t.Errorf("stream %q holds %d events, want %d", id, len(got), n)
	}
}

func drain(t *testing.T, it *eventsourcing.Iterator[*eventsourcing.Envelope]) []*eventsourcing.Envelope {
	t.Helper()
	out, err := it.All()
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func versions(envs []*eventsourcing.Envelope) []uint64 {
	out := make([]uint64, len(envs))
	for i, env := range envs {
		out[i] = env.Version
	}
	return out
}

func payloadNs(t *testing.T, envs []*eventsourcing.Envelope) []int {
	t.Helper()
	out := make([]int, len(envs))
	for i, env := range envs {
		ev, ok := env.Event.(*acceptanceEvent)
		if !ok {
			t.Fatalf("event %d is %T, want %T", i, env.Event, ev)
		}
		out[i] = ev.N
	}
	return out
}

func label(id, first, second string) string {
	switch id {
	case first:
		return "first"
	case second:
		return "second"
	}
	return id
}
