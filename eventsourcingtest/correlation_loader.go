package eventsourcingtest

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

type correlationAcceptance struct {
	store  eventsourcing.EventStore
	reader eventsourcing.CorrelationLoader
}

// CorrelationLoaderAcceptanceTest runs the [eventsourcing.CorrelationLoader]
// contract against reader as a set of subtests. store must be the store
// reader reads from; the suite appends to it through Save and checks what
// LoadCorrelation returns.
//
// Like [AcceptanceTest], it may run against a store that already holds
// events: every chain it saves is named with a fresh UUID.
//
// The suite checks that:
//
//   - a chain saved across several streams, interleaved with saves of
//     other chains, comes back whole and in ascending GlobalPosition order;
//   - every field of a loaded event round-trips, its chain included;
//   - a correlation ID no event carries yields no events and no error;
//   - the empty correlation ID returns an error wrapping
//     [eventsourcing.ErrEmptyCorrelationID];
//   - closing the iterator before reading it to the end succeeds.
func CorrelationLoaderAcceptanceTest(t *testing.T, store eventsourcing.EventStore, reader eventsourcing.CorrelationLoader) {
	t.Helper()

	c := &correlationAcceptance{store: store, reader: reader}
	a := &acceptance{store: store}
	a.group(t, "CorrelationLoader", map[string]func(*testing.T){
		"AcrossStreams": c.acrossStreams,
		"RoundTrip":     c.roundTrip,
		"Unknown":       c.unknown,
		"EmptyID":       c.emptyID,
		"CloseEarly":    c.closeEarly,
	})
}

func (c *correlationAcceptance) acrossStreams(t *testing.T) {
	chain, other := "chain-"+uuid.NewString(), "chain-"+uuid.NewString()
	a, b := newStreamID(), newStreamID()
	c.save(t, chained(envelopes(a, 1, 2), chain), eventsourcing.NoStream{})
	c.save(t, chained(envelopes(b, 1, 1), other), eventsourcing.NoStream{})
	c.save(t, chained(envelopes(b, 2, 1), chain), eventsourcing.Revision(1))
	c.save(t, chained(envelopes(a, 3, 1), other), eventsourcing.Revision(2))
	c.save(t, chained(envelopes(a, 4, 1), chain), eventsourcing.Revision(3))

	got := c.load(t, chain)
	if l := streamVersions(got, a, b); !slices.Equal(l, []string{"a@1", "a@2", "b@2", "a@4"}) {
		t.Errorf("LoadCorrelation(%q) = %v, want [a@1 a@2 b@2 a@4]", chain, l)
	}
	for i := 1; i < len(got); i++ {
		if got[i].GlobalPosition <= got[i-1].GlobalPosition {
			t.Errorf("GlobalPositions %d then %d, want strictly ascending", got[i-1].GlobalPosition, got[i].GlobalPosition)
		}
	}
}

func (c *correlationAcceptance) roundTrip(t *testing.T) {
	chain := "chain-" + uuid.NewString()
	id := newStreamID()
	in := chained(envelopes(id, 1, 2), chain)
	in[0].CausationID = ""
	in[1].CausationID = in[0].EventID.String()
	c.save(t, in, eventsourcing.NoStream{})

	got := c.load(t, chain)
	if len(got) != 2 {
		t.Fatalf("LoadCorrelation returned %d events, want 2", len(got))
	}
	for i, out := range got {
		if want := uint64(i + 1); out.EventID != in[i].EventID || out.StreamID != id || out.Version != want {
			t.Errorf("event %d = %v %q@%d, want %v %q@%d", i, out.EventID, out.StreamID, out.Version, in[i].EventID, id, want)
		}
		if out.CorrelationID != chain || out.CausationID != in[i].CausationID {
			t.Errorf("event %d chain = %q/%q, want %q/%q", i, out.CorrelationID, out.CausationID, chain, in[i].CausationID)
		}
		if out.GlobalPosition == 0 {
			t.Errorf("event %d GlobalPosition = 0, want the event's position in the log", i)
		}
		ev, ok := out.Event.(*acceptanceEvent)
		if !ok {
			t.Fatalf("event %d Event = %T, want %T", i, out.Event, ev)
		}
		if want := in[i].Event.(*acceptanceEvent); *ev != *want {
			t.Errorf("event %d Event = %+v, want %+v", i, *ev, *want)
		}
		if got, want := out.Metadata["acceptance"], in[i].Metadata["acceptance"]; got != want {
			t.Errorf(`event %d Metadata["acceptance"] = %v, want %v`, i, got, want)
		}
	}
}

func (c *correlationAcceptance) unknown(t *testing.T) {
	chain := "chain-" + uuid.NewString()
	if got := c.load(t, chain); len(got) != 0 {
		t.Errorf("LoadCorrelation of an unknown chain returned %d events, want 0", len(got))
	}
}

func (c *correlationAcceptance) emptyID(t *testing.T) {
	it, err := c.reader.LoadCorrelation(t.Context(), "")
	if err == nil {
		_, err = it.All()
	}
	if !errors.Is(err, eventsourcing.ErrEmptyCorrelationID) {
		t.Errorf("LoadCorrelation(\"\"): err = %v, want %v", err, eventsourcing.ErrEmptyCorrelationID)
	}
}

func (c *correlationAcceptance) closeEarly(t *testing.T) {
	chain := "chain-" + uuid.NewString()
	c.save(t, chained(envelopes(newStreamID(), 1, 3), chain), eventsourcing.NoStream{})

	it, err := c.reader.LoadCorrelation(t.Context(), chain)
	if err != nil {
		t.Fatalf("LoadCorrelation(%q): %v", chain, err)
	}
	if !it.Next() {
		t.Fatalf("LoadCorrelation(%q) yielded nothing: %v", chain, it.Err())
	}
	if err := it.Close(); err != nil {
		t.Errorf("Close after one event: %v", err)
	}
}

// --- helpers ---

func (c *correlationAcceptance) save(t *testing.T, events []eventsourcing.Envelope, state eventsourcing.StreamState) {
	t.Helper()
	if _, err := c.store.Save(t.Context(), events, state); err != nil {
		t.Fatalf("Save(%v) to %q: %v", state, events[0].StreamID, err)
	}
}

func (c *correlationAcceptance) load(t *testing.T, chain string) []*eventsourcing.Envelope {
	t.Helper()
	it, err := c.reader.LoadCorrelation(t.Context(), chain)
	if err != nil {
		t.Fatalf("LoadCorrelation(%q): %v", chain, err)
	}
	return drain(t, it)
}

// chained sets every envelope's CorrelationID to chain and returns envs.
func chained(envs []eventsourcing.Envelope, chain string) []eventsourcing.Envelope {
	for i := range envs {
		envs[i].CorrelationID = chain
	}
	return envs
}
