package slicetest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
)

// log wraps the memory event store with the two things every slice needs:
// seeding past events and reading what was recorded after a point.
type log struct {
	t     testing.TB
	store *memory.EventStore
	opts  options
}

func newLog(t testing.TB, opts options) *log {
	t.Helper()
	store := memory.NewEventStore()
	t.Cleanup(func() { _ = store.Close() })
	return &log{t: t, store: store, opts: opts}
}

// seed appends events as past history, in the order given, and returns
// the global position after them. A run of consecutive events on one
// stream is one Save, so the global log reads exactly as given, also when
// streams interleave.
func (l *log) seed(events []eventsourcing.Event) uint64 {
	l.t.Helper()
	var run []eventsourcing.Envelope
	flush := func() {
		if len(run) == 0 {
			return
		}
		if _, err := l.store.Save(l.t.Context(), run, eventsourcing.Any{}); err != nil {
			l.t.Fatalf("slicetest: given events for stream %q: %v", run[0].StreamID, err)
		}
		run = nil
	}
	for _, ev := range events {
		id := l.opts.streamID(ev)
		if len(run) > 0 && run[0].StreamID != id {
			flush()
		}
		run = append(run, eventsourcing.Envelope{
			EventID:  uuid.New(),
			StreamID: id,
			Event:    ev,
			Metadata: map[string]any{},
		})
	}
	flush()
	return l.head()
}

func (l *log) head() uint64 {
	l.t.Helper()
	head, err := l.store.Head(l.t.Context())
	if err != nil {
		l.t.Fatalf("slicetest: head: %v", err)
	}
	return head
}

// since returns every event recorded after position, in global order.
func (l *log) since(position uint64) []eventsourcing.Event {
	l.t.Helper()
	envs := l.envelopesSince(position)
	out := make([]eventsourcing.Event, len(envs))
	for i, env := range envs {
		out[i] = env.Event
	}
	return out
}

// envelopesSince is since with the envelopes kept, for assertions on
// version, correlation or metadata.
func (l *log) envelopesSince(position uint64) []*eventsourcing.Envelope {
	l.t.Helper()
	var out []*eventsourcing.Envelope
	after := position
	for {
		res, err := l.store.ReadAll(l.t.Context(), after, eventsourcing.ReadOptions{})
		if err != nil {
			l.t.Fatalf("slicetest: read events: %v", err)
		}
		out = append(out, res.Events...)
		if res.AtEnd || res.Next == after {
			return out
		}
		after = res.Next
	}
}
