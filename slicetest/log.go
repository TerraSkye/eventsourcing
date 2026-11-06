package slicetest

import (
	"context"
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

// seed appends events as past history, one Save per stream in the order
// given, and returns the global position after them.
func (l *log) seed(events []eventsourcing.Event) uint64 {
	l.t.Helper()
	ctx := context.Background()
	// Keep the given order within a stream, and save stream by stream in
	// first-appearance order so the global log reads as written.
	var order []string
	byStream := map[string][]eventsourcing.Envelope{}
	for _, ev := range events {
		id := l.opts.streamID(ev)
		if _, ok := byStream[id]; !ok {
			order = append(order, id)
		}
		byStream[id] = append(byStream[id], eventsourcing.Envelope{
			EventID:  uuid.New(),
			StreamID: id,
			Event:    ev,
			Metadata: map[string]any{},
		})
	}
	for _, id := range order {
		if _, err := l.store.Save(ctx, byStream[id], eventsourcing.Any{}); err != nil {
			l.t.Fatalf("slicetest: given events for stream %q: %v", id, err)
		}
	}
	return l.head()
}

func (l *log) head() uint64 {
	l.t.Helper()
	head, err := l.store.Head(context.Background())
	if err != nil {
		l.t.Fatalf("slicetest: head: %v", err)
	}
	return head
}

// since returns every event recorded after position, in global order.
func (l *log) since(position uint64) []eventsourcing.Event {
	l.t.Helper()
	ctx := context.Background()
	var out []eventsourcing.Event
	after := position
	for {
		res, err := l.store.ReadAll(ctx, after, eventsourcing.ReadOptions{})
		if err != nil {
			l.t.Fatalf("slicetest: read events: %v", err)
		}
		for _, env := range res.Events {
			out = append(out, env.Event)
		}
		if res.AtEnd || res.Next == after {
			return out
		}
		after = res.Next
	}
}

// envelopesSince is since with the envelopes kept, for assertions on
// metadata.
func (l *log) envelopesSince(position uint64) []*eventsourcing.Envelope {
	l.t.Helper()
	ctx := context.Background()
	var out []*eventsourcing.Envelope
	after := position
	for {
		res, err := l.store.ReadAll(ctx, after, eventsourcing.ReadOptions{})
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
