package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
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
	for iter.Next() {
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

// TestSave_GlobalSequenceConflictRollsBackBatch is a regression test for the
// combination of two behaviors Save relies on for correctness under
// concurrent writers sharing a directory: a global version collision must
// surface as a [cqrs.StreamRevisionConflictError] a caller can recognize and
// retry (the same way NewCommandHandler's retry loop already does for
// stream-level conflicts), and the batch that hit it must not leave any of
// its earlier, individually-successful writes behind — Save either commits
// the whole batch or none of it.
func TestSave_GlobalSequenceConflictRollsBackBatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	got := make(chan *cqrs.Envelope, 10)
	go func() {
		for ev := range store.Events() {
			got <- ev
		}
	}()

	// Pre-claim the global version the second event in the batch below will
	// be assigned (globalSeq starts at 0, so the first event takes 1 and the
	// second takes 2), simulating a peer instance that won that race.
	collidingPath := filepath.Join(dir, allDirName, eventFileName(2, "allCollisionEvent"))
	if err := os.Symlink("/nonexistent", collidingPath); err != nil {
		t.Fatalf("pre-create colliding symlink: %v", err)
	}

	_, err = store.Save(ctx, []cqrs.Envelope{
		envelopeFor("cart-3", 0, "first"),
		envelopeFor("cart-3", 1, "second"),
	}, cqrs.NoStream{})
	if err == nil {
		t.Fatal("expected a conflict error, got nil")
	}
	var conflict *cqrs.StreamRevisionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *cqrs.StreamRevisionConflictError, got %T: %v", err, err)
	}

	// The first event's own write succeeded before the second one collided;
	// it must have been rolled back along with the second, not left behind.
	entries, _ := os.ReadDir(store.streamDir("cart-3"))
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("batch not fully rolled back, stream dir has: %v", names)
	}

	select {
	case ev := <-got:
		t.Fatalf("Events received an envelope from a failed batch: %+v", ev)
	default:
	}
}

// TestLoadStreamFrom_NeverCreatedStream is a regression test for GitHub
// issue #41: loadFromDir called os.ReadDir on a stream's directory before
// looking at the requested StreamState. A stream's directory is only
// created lazily inside Save, on its first successful write, so loading a
// brand-new aggregate ID failed with a raw *fs.PathError for every
// StreamState, including Any{} and NoStream{}, which both document success
// in this exact situation.
func TestLoadStreamFrom_NeverCreatedStream(t *testing.T) {
	ctx := context.Background()

	t.Run("Any_should_yield_empty_iterator", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		iter, err := store.LoadStreamFrom(ctx, "brand-new-aggregate", cqrs.Any{})
		if err != nil {
			t.Fatalf("Any{} on a never-saved stream: expected no error, got: %v", err)
		}
		if iter.Next() {
			t.Fatalf("expected an empty iterator for a never-saved stream")
		}
	})

	t.Run("NoStream_should_yield_empty_iterator", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		iter, err := store.LoadStreamFrom(ctx, "brand-new-aggregate", cqrs.NoStream{})
		if err != nil {
			t.Fatalf("NoStream{} on a never-saved stream: expected no error (stream genuinely does not exist), got: %v", err)
		}
		if iter.Next() {
			t.Fatalf("expected an empty iterator for a never-saved stream")
		}
	})

	t.Run("StreamExists_should_wrap_ErrStreamNotFound", func(t *testing.T) {
		dir := t.TempDir()
		store, err := NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		_, err = store.LoadStreamFrom(ctx, "brand-new-aggregate", cqrs.StreamExists{})
		if err == nil {
			t.Fatalf("expected an error for StreamExists{} on a never-saved stream")
		}
		if !errors.Is(err, cqrs.ErrStreamNotFound) {
			t.Fatalf("expected err to wrap cqrs.ErrStreamNotFound, got: %v", err)
		}
	})
}

// TestLoadStreamFrom_RevisionAtStreamHead is a regression test for GitHub
// issue #42: loadFromDir's Revision guard used >= where a start index needs
// >, so asking for the revision a stream is currently at — "I am caught up,
// give me anything newer" — returned ErrInvalidRevision instead of an empty
// iterator. Revision(0) against an empty-but-existing stream made it
// impossible to create a new aggregate through NewCommandHandler configured
// with WithStreamState(Revision(0)).
func TestLoadStreamFrom_RevisionAtStreamHead(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		eventCount int
	}{
		{name: "fresh stream, revision 0", eventCount: 0},
		{name: "one event, revision 1", eventCount: 1},
		{name: "three events, revision 3", eventCount: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewFileStore(dir)
			if err != nil {
				t.Fatalf("NewFileStore: %v", err)
			}
			defer store.Close()

			if tt.eventCount > 0 {
				events := make([]cqrs.Envelope, tt.eventCount)
				for i := range events {
					events[i] = envelopeFor("order-1", uint64(i), "item")
				}
				if _, err := store.Save(ctx, events, cqrs.Any{}); err != nil {
					t.Fatalf("setup save: %v", err)
				}
			} else {
				// Isolate the off-by-one from the unrelated "stream directory was
				// never created" case: create the (empty) stream directory directly.
				if err := os.MkdirAll(store.streamDir("order-1"), 0o755); err != nil {
					t.Fatalf("setup mkdir: %v", err)
				}
			}

			iter, err := store.LoadStreamFrom(ctx, "order-1", cqrs.Revision(tt.eventCount))
			if err != nil {
				t.Fatalf("LoadStreamFrom(Revision(%d)) on a stream with %d events: "+
					"expected an empty iterator, got error: %v", tt.eventCount, tt.eventCount, err)
			}

			count := 0
			for iter.Next() {
				count++
			}
			if err := iter.Err(); err != nil {
				t.Fatalf("iterator error: %v", err)
			}
			if count != 0 {
				t.Errorf("expected 0 events past the head, got %d", count)
			}
		})
	}
}

// TestLoadFromAll_RevisionAtHead is the LoadFromAll counterpart of
// TestLoadStreamFrom_RevisionAtStreamHead: the same off-by-one guard backs
// both, so it affects global-log reads too.
func TestLoadFromAll_RevisionAtHead(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name       string
		eventCount int
	}{
		{name: "empty store, position 0", eventCount: 0},
		{name: "two events, position 2", eventCount: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := NewFileStore(dir)
			if err != nil {
				t.Fatalf("NewFileStore: %v", err)
			}
			defer store.Close()

			for i := 0; i < tt.eventCount; i++ {
				if _, err := store.Save(ctx, []cqrs.Envelope{
					envelopeFor("order-1", uint64(i), "item"),
				}, cqrs.Any{}); err != nil {
					t.Fatalf("setup save: %v", err)
				}
			}

			iter, err := store.LoadFromAll(ctx, cqrs.Revision(tt.eventCount))
			if err != nil {
				t.Fatalf("LoadFromAll(Revision(%d)) with %d events stored: "+
					"expected an empty iterator, got error: %v", tt.eventCount, tt.eventCount, err)
			}

			count := 0
			for iter.Next() {
				count++
			}
			if err := iter.Err(); err != nil {
				t.Fatalf("iterator error: %v", err)
			}
			if count != 0 {
				t.Errorf("expected 0 events past the head, got %d", count)
			}
		})
	}
}

// TestLoadStreamFrom_RevisionExcludesAlreadySeenEvent documents a bug: unlike
// [eventstore/memory.MemoryStore] and [eventstore/postgres]'s implementation
// of the same [cqrs.EventStore] interface, FilesStore.LoadStreamFrom treats
// Revision(N) as an INCLUSIVE start position (returns the event whose
// Version equals N again) instead of an EXCLUSIVE one (only events with
// Version > N).
//
// This matters because [cqrs.NewCommandHandler] (command_handler.go) always
// numbers a stream's events starting at 1 — nextVersion := lastVersion + 1
// where lastVersion starts at its zero value — and, on a save conflict, sets
// revision to Revision(lastly-evolved event's Version) before reloading and
// retrying incrementally rather than from scratch. That retry path depends
// on LoadStreamFrom(id, Revision(N)) excluding the event already folded into
// state; FilesStore including it again would double-apply that event to the
// aggregate's state on every conflict retry.
//
// See .bug/eventstore-file-loadstreamfrom-revision-off-by-one-includes-seen-event.md.
func TestLoadStreamFrom_RevisionExcludesAlreadySeenEvent(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	// Mirror NewCommandHandler's real numbering convention: the first event
	// saved to a stream gets Version 1, not 0.
	events := []cqrs.Envelope{
		envelopeFor("order-1", 1, "item-1"),
		envelopeFor("order-1", 2, "item-2"),
	}
	if _, err := store.Save(ctx, events, cqrs.Any{}); err != nil {
		t.Fatalf("setup save: %v", err)
	}

	// "I have already evolved the event at Version 1; give me anything
	// newer" must return only the Version-2 event.
	iter, err := store.LoadStreamFrom(ctx, "order-1", cqrs.Revision(1))
	if err != nil {
		t.Fatalf("LoadStreamFrom(Revision(1)): %v", err)
	}

	var versions []uint64
	for iter.Next() {
		versions = append(versions, iter.Value().Version)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if len(versions) != 1 || versions[0] != 2 {
		t.Errorf("LoadStreamFrom(Revision(1)) = versions %v, want [2] (the already-seen Version-1 event must not be repeated)", versions)
	}
}

// TestLoadStream_VersionAbove9999999999SortsBeforeEarlierEvents is a
// regression test for GitHub issue #146: loadFromDir yields events in
// os.ReadDir's lexical filename order, and Save used to name files
// "%010d-<EventType>.json".
// %010d only sets a minimum width, so a Version of 10,000,000,000 or more
// got an 11-digit name, which sorts by its leading "1" before a 10-digit
// name such as 9999999999's, and LoadStream yielded that later event first.
//
// Save never assigns Version itself (see its TODO), so a caller can reach
// a version this large without appending ten billion events.
func TestLoadStream_VersionAbove9999999999SortsBeforeEarlierEvents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	// Two sequential appends whose versions cross from 10 digits to 11.
	if _, err := store.Save(ctx, []cqrs.Envelope{envelopeFor("order-1", 9999999999, "first")}, cqrs.Any{}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := store.Save(ctx, []cqrs.Envelope{envelopeFor("order-1", 10000000000, "second")}, cqrs.Any{}); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}

	var versions []uint64
	for iter.Next() {
		versions = append(versions, iter.Value().Version)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if len(versions) != 2 || versions[0] != 9999999999 || versions[1] != 10000000000 {
		t.Errorf("LoadStream order = %v, want [9999999999 10000000000] (events in the order they were appended)", versions)
	}
}

// TestLoadFromAll_GlobalVersionAbove9999999999KeepsOrder is the LoadFromAll
// counterpart of TestLoadStream_VersionAbove9999999999SortsBeforeEarlierEvents:
// allDir's symlinks are named after GlobalVersion by the same format, so the
// global log had the same ordering bug once it crossed 10 digits.
func TestLoadFromAll_GlobalVersionAbove9999999999KeepsOrder(t *testing.T) {
	ctx := context.Background()

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	// The next two appends are assigned global versions 9999999999 and
	// 10000000000.
	store.mu.Lock()
	store.globalSeq = 9999999998
	store.mu.Unlock()

	if _, err := store.Save(ctx, []cqrs.Envelope{envelopeFor("order-1", 1, "first")}, cqrs.Any{}); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	if _, err := store.Save(ctx, []cqrs.Envelope{envelopeFor("order-2", 1, "second")}, cqrs.Any{}); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Any{})
	if err != nil {
		t.Fatalf("LoadFromAll: %v", err)
	}

	var versions []uint64
	for iter.Next() {
		versions = append(versions, iter.Value().GlobalVersion)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if len(versions) != 2 || versions[0] != 9999999999 || versions[1] != 10000000000 {
		t.Errorf("LoadFromAll order = %v, want [9999999999 10000000000] (events in the order they were appended)", versions)
	}
}

func TestAcceptance(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	eventsourcingtest.AcceptanceTest(t, store)
}

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

	store, err := NewFileStore(dir)
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

// TestSave_UnreadableStreamDirBypassesConcurrencyCheck is a regression test
// for GitHub issue #144: Save discarded the error from listing the stream
// directory and read a failed listing as an empty stream. A directory that
// can be written but not listed then let a NoStream{} append through against
// a stream that already had events, overwriting the event on disk that
// shared its Version.
func TestSave_UnreadableStreamDirBypassesConcurrencyCheck(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("test relies on permission enforcement; not meaningful running as root")
	}

	ctx := context.Background()

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	streamID := "cart-1"

	if _, err := store.Save(ctx, []cqrs.Envelope{
		envelopeFor(streamID, 0, "first"),
	}, cqrs.NoStream{}); err != nil {
		t.Fatalf("setup save: %v", err)
	}

	// Write and execute without read: files can still be created in the
	// directory, but it cannot be listed.
	sdir := store.streamDir(streamID)
	if err := os.Chmod(sdir, 0o300); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sdir, 0o755) }) // let TempDir cleanup walk it again

	if _, err := os.ReadDir(sdir); err == nil {
		t.Fatalf("expected ReadDir to fail with the directory at 0300, got no error")
	}

	if _, err := store.Save(ctx, []cqrs.Envelope{
		envelopeFor(streamID, 0, "second"),
	}, cqrs.NoStream{}); err == nil {
		t.Fatalf("Save with NoStream{} against an existing (but unlistable) stream unexpectedly succeeded")
	}

	if err := os.Chmod(sdir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	it, err := store.LoadStream(ctx, streamID)
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	var got []string
	for it.Next() {
		got = append(got, it.Value().Event.(*allCollisionEvent).Name)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if len(got) != 1 || got[0] != "first" {
		t.Fatalf("stream events = %v, want [first]", got)
	}
}
