package file_test

import (
	"context"
	"os"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	file "github.com/terraskye/eventsourcing/eventstore/file"
)

type aliasChannelTestEvent struct {
	ID    string
	Value string
}

func (e aliasChannelTestEvent) AggregateID() string { return e.ID }
func (e aliasChannelTestEvent) EventType() string   { return "aliasChannelTestEvent" }

// TestFilesStore_EventsChannelAliasesCallerSlice checks that envelopes
// delivered on Events() are not affected by the caller reusing the slice it
// passed to Save.
func TestFilesStore_EventsChannelAliasesCallerSlice(t *testing.T) {
	dir, err := os.MkdirTemp("", "filestore-alias")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store, err := file.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()

	// A single-element buffer reused across independent Save calls, as a
	// caller might do to avoid allocating a new slice per aggregate.
	buf := make([]cqrs.Envelope, 1)

	for i, id := range []string{"agg-1", "agg-2", "agg-3"} {
		buf[0] = cqrs.Envelope{
			StreamID: id,
			Event:    aliasChannelTestEvent{ID: id, Value: id},
			Version:  uint64(i),
		}
		if _, err := store.Save(ctx, buf, cqrs.NoStream{}); err != nil {
			t.Fatalf("save %q failed: %v", id, err)
		}
	}

	// Drain the Events() channel after all Save calls; if it aliases the
	// caller's reused buffer, every received envelope reflects the LAST
	// Save call's data instead of its own, even though the on-disk copy
	// (written via json.Marshal before this loop runs) is unaffected.
	for _, want := range []string{"agg-1", "agg-2", "agg-3"} {
		select {
		case env := <-store.Events():
			got := env.Event.(aliasChannelTestEvent).Value
			if got != want {
				t.Errorf("expected event value %q, got %q (StreamID recorded as %q)", want, got, env.StreamID)
			}
		default:
			t.Fatalf("expected an event on the bus for %q", want)
		}
	}
}
