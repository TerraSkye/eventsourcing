package memory_test

import (
	"context"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventstore/memory"
)

// Save copied each envelope into the store before assigning its
// GlobalVersion, and assigned it to the caller's slice instead, so every
// stored event read back with GlobalVersion 0.
func TestSave_StoresGlobalVersion(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1"}),
	}, cqrs.NoStream{}); err != nil {
		t.Fatalf("save order-1: %v", err)
	}
	if _, err := store.Save(ctx, []cqrs.Envelope{
		newEnvelope("order-2", OrderCreated{OrderID: "order-2"}),
	}, cqrs.NoStream{}); err != nil {
		t.Fatalf("save order-2: %v", err)
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Any{})
	if err != nil {
		t.Fatalf("LoadFromAll: %v", err)
	}
	all := collectAll(t, iter)
	if len(all) != 3 {
		t.Fatalf("LoadFromAll returned %d events, want 3", len(all))
	}
	for i, env := range all {
		if want := uint64(i + 1); env.GlobalVersion != want {
			t.Errorf("LoadFromAll event %d: GlobalVersion = %d, want %d", i, env.GlobalVersion, want)
		}
	}

	iter, err = store.LoadStream(ctx, "order-2")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	stream := collectAll(t, iter)
	if len(stream) != 1 || stream[0].GlobalVersion != 3 {
		t.Errorf("LoadStream(order-2) = %v, want one event with GlobalVersion 3", stream)
	}
}

// Save sent &events[i] on the Events() channel, a pointer into the caller's
// slice. A caller reusing that slice across Save calls saw every envelope
// still buffered on the channel rewritten to the latest call's data.
func TestSave_EventsChannelDoesNotAliasCallerSlice(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()
	ctx := context.Background()

	// A single-element buffer reused across independent Save calls, as a
	// caller might do to avoid allocating a new slice per aggregate.
	buf := make([]cqrs.Envelope, 1)
	ids := []string{"order-1", "order-2", "order-3"}
	for _, id := range ids {
		buf[0] = newEnvelope(id, OrderCreated{OrderID: id})
		if _, err := store.Save(ctx, buf, cqrs.NoStream{}); err != nil {
			t.Fatalf("save %q: %v", id, err)
		}
	}

	for i, want := range ids {
		select {
		case env := <-store.Events():
			if got := env.Event.(OrderCreated).OrderID; got != want {
				t.Errorf("Events() envelope %d: OrderID = %q, want %q", i, got, want)
			}
			if wantGV := uint64(i + 1); env.GlobalVersion != wantGV {
				t.Errorf("Events() envelope %d: GlobalVersion = %d, want %d", i, env.GlobalVersion, wantGV)
			}
		default:
			t.Fatalf("expected an event on Events() for %q", want)
		}
	}
}
