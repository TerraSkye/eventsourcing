package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
)

type allCollisionEvent struct {
	Name string
}

func (e allCollisionEvent) AggregateID() string { return e.Name }
func (e allCollisionEvent) EventType() string   { return "allCollisionEvent" }

func init() {
	cqrs.RegisterEventByType(func() cqrs.Event { return &allCollisionEvent{} })
}

func envelopeFor(streamID string, version uint64, name string) cqrs.Envelope {
	return cqrs.Envelope{
		StreamID: streamID,
		Event:    allCollisionEvent{Name: name},
		Version:  version,
	}
}

// TestSave_StreamNamedAllCollidesWithGlobalDir is a regression test for
// GitHub issue #38: streamDir(id) had no guard against the reserved "all"
// directory name FilesStore uses for its global symlink fan-in, so a stream
// literally named "all" read and wrote that same directory. Save with
// NoStream{} against a brand-new stream "all" incorrectly failed with
// "stream already exists" once any other stream had saved at least one
// event, since the version count was polluted by every other stream's
// symlinks.
func TestSave_StreamNamedAllCollidesWithGlobalDir(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	// An unrelated stream writes one event; this also symlinks into all/.
	if _, err := store.Save(ctx, []cqrs.Envelope{
		envelopeFor("cart-1", 0, "unrelated"),
	}, cqrs.NoStream{}); err != nil {
		t.Fatalf("setup save to cart-1: %v", err)
	}

	// "all" has never been used as a real stream ID before. NoStream{} must
	// succeed, exactly as it would for any other never-saved aggregate ID.
	res, err := store.Save(ctx, []cqrs.Envelope{
		envelopeFor("all", 0, "first-in-all"),
	}, cqrs.NoStream{})
	if err != nil {
		t.Fatalf("Save(NoStream{}) for brand-new stream %q: expected success, got error: %v", "all", err)
	}
	if !res.Successful {
		t.Fatalf("Save(NoStream{}) for brand-new stream %q: expected Successful=true", "all")
	}

	// The stream "all" should now be loadable on its own, containing only
	// the one event just saved to it — not polluted by cart-1's event.
	iter, err := store.LoadStream(ctx, "all")
	if err != nil {
		t.Fatalf("LoadStream(%q): %v", "all", err)
	}
	var envs []*cqrs.Envelope
	for iter.Next(ctx) {
		envs = append(envs, iter.Value())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate LoadStream(%q): %v", "all", err)
	}
	if len(envs) != 1 {
		t.Fatalf("LoadStream(%q) = %d events, want 1", "all", len(envs))
	}
	if ev, ok := envs[0].Event.(*allCollisionEvent); !ok || ev.Name != "first-in-all" {
		t.Fatalf("LoadStream(%q)[0].Event = %#v, want *allCollisionEvent{Name: \"first-in-all\"}", "all", envs[0].Event)
	}
}

// TestFileStoreClose_CalledTwice_Panics is a regression test for GitHub
// issue #39: Close unconditionally called close(f.bus) with no guard, so
// calling it a second time panicked with "close of closed channel",
// contrary to the EventStore contract that Close must be idempotent.
func TestFileStoreClose_CalledTwice_Panics(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("first Close: unexpected error: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second Close panicked: %v (EventStore godoc requires Close to be idempotent)", r)
		}
	}()

	if err := store.Close(); err != nil {
		t.Errorf("second Close: unexpected error: %v", err)
	}
}

// TestFileStoreSave_AfterClose_Panics is a regression test for GitHub issue
// #39: Save's non-blocking send to f.bus only guarded against a full
// channel, not a closed one, so any Save call after Close panicked with
// "send on closed channel" instead of returning an error.
func TestFileStoreSave_AfterClose_Panics(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Save after Close panicked: %v", r)
		}
	}()

	_, err = store.Save(context.Background(), []cqrs.Envelope{
		envelopeFor("order-1", 0, "after-close"),
	}, cqrs.NoStream{})
	if err == nil {
		t.Error("expected an error saving to a closed store, got nil")
	}
}

// TestFileStoreGlobalSequenceSurvivesReopen is a regression test for GitHub
// issue #40: globalSeq started at 0 on every NewFileStore call and was never
// recovered from the events already on disk, so reopening a store over an
// existing directory re-issued global versions that were already taken —
// causing Save to fail with "file exists" (same event type as an existing
// event) or silently produce duplicate GlobalVersion values (different
// event type).
func TestFileStoreGlobalSequenceSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	res, err := first.Save(ctx, []cqrs.Envelope{envelopeFor("cart-1", 0, "first")}, cqrs.NoStream{})
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if !res.Successful {
		t.Fatalf("first Save unsuccessful")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same directory, as a process restart would.
	second, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("reopen NewFileStore: %v", err)
	}
	defer second.Close()

	res, err = second.Save(ctx, []cqrs.Envelope{envelopeFor("cart-2", 0, "second")}, cqrs.NoStream{})
	if err != nil {
		t.Fatalf("Save after reopen: %v", err)
	}
	if !res.Successful {
		t.Fatalf("Save after reopen unsuccessful")
	}

	// Both events must be reachable from all/ with distinct global versions.
	entries, err := os.ReadDir(filepath.Join(dir, allDirName))
	if err != nil {
		t.Fatalf("ReadDir(all): %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("all/ has %d entries %v, want 2", len(entries), names)
	}
}
