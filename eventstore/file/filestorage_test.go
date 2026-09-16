package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
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
	collidingPath := filepath.Join(dir, allDirName, fmt.Sprintf("%010d-%s.json", 2, "allCollisionEvent"))
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
		if iter.Next(ctx) {
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
		if iter.Next(ctx) {
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
			for iter.Next(ctx) {
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
			for iter.Next(ctx) {
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
	for iter.Next(ctx) {
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
// regression test for a bug where loadFromDir relies on os.ReadDir's
// lexical filename sort to reproduce append order, but Save names each
// event's file "%010d-<EventType>.json" from its own Version field. %010d
// only zero-pads up to 10 digits; a Version of 10,000,000,000 or higher
// needs an 11th digit, so its filename is longer than — and therefore, per
// Go's byte-wise string comparison, lexically less than — any file for a
// Version that still fits in 10 digits, even one appended long before it.
// LoadStream then yields that later event first.
//
// This file's own pre-existing TODO on Save already documents that the
// Version field is caller-supplied and never assigned or validated by Save
// itself, so a real Version this large is reachable without needing to
// actually append ten billion events to trigger it.
//
// See .bug/eventstore-file-large-version-lexical-sort-breaks-order.md.
func TestLoadStream_VersionAbove9999999999SortsBeforeEarlierEvents(t *testing.T) {

	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	// Two Save calls, each appending one event, in ascending Version order —
	// exactly like two ordinary sequential appends, just at a version range
	// that crosses the 10-digit boundary %010d pads to.
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
	for iter.Next(ctx) {
		versions = append(versions, iter.Value().Version)
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}

	if len(versions) != 2 || versions[0] != 9999999999 || versions[1] != 10000000000 {
		t.Errorf("LoadStream order = %v, want [9999999999 10000000000] (events in the order they were appended)", versions)
	}
}

// This file covers the serialize -> persist -> deserialize roundtrip for the
// file store: FilesStore.Save json.Marshal's the event into a file on disk
// and LoadStream/LoadFromAll rebuild it from the registry. The shared
// registry/JSON half of that contract is pinned in the root package's
// serialization_test.go; here the bytes really go through the filesystem.

type money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

type lineItem struct {
	SKU   string   `json:"sku"`
	Qty   int      `json:"qty"`
	Price money    `json:"price"`
	Notes []string `json:"notes"`
}

type shippingAddress struct {
	Street  string `json:"street"`
	Country string `json:"country"`
}

// roundtripEvent mixes nested structs, a slice of structs, set and nil
// pointers, maps, a uuid.UUID and a nanosecond-precision time.Time.
type roundtripEvent struct {
	OrderID  string            `json:"order_id"`
	TraceID  uuid.UUID         `json:"trace_id"`
	PlacedAt time.Time         `json:"placed_at"`
	Total    money             `json:"total"`
	Items    []lineItem        `json:"items"`
	ShipTo   *shippingAddress  `json:"ship_to"`
	BillTo   *shippingAddress  `json:"bill_to"`
	Labels   map[string]string `json:"labels"`
	Discount float64           `json:"discount"`
	Rushed   bool              `json:"rushed"`
}

func (e *roundtripEvent) AggregateID() string { return e.OrderID }
func (e *roundtripEvent) EventType() string   { return "roundtripEvent" }

func init() {
	cqrs.RegisterEvent(&roundtripEvent{})
}

func newRoundtripEvent(orderID string) *roundtripEvent {
	return &roundtripEvent{
		OrderID:  orderID,
		TraceID:  uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		PlacedAt: time.Date(2024, 3, 1, 12, 34, 56, 123456789, time.UTC),
		Total:    money{Amount: 4999, Currency: "EUR"},
		Items: []lineItem{
			{SKU: "WIDGET-1", Qty: 2, Price: money{Amount: 1999, Currency: "EUR"}, Notes: []string{"gift wrap"}},
			{SKU: "WIDGET-2", Qty: 1, Price: money{Amount: 1001, Currency: "EUR"}},
		},
		ShipTo:   &shippingAddress{Street: "Keizersgracht 1", Country: "NL"},
		BillTo:   nil,
		Labels:   map[string]string{"channel": "web"},
		Discount: 12.5,
		Rushed:   true,
	}
}

// TestSerializationRoundtrip_SaveAndLoadStream asserts that a rich event
// survives Save -> disk -> LoadStream unchanged, and that the envelope
// metadata around it (EventID, Version, OccurredAt, Metadata) survives too.
func TestSerializationRoundtrip_SaveAndLoadStream(t *testing.T) {
	ctx := context.Background()

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	want := newRoundtripEvent("order-1")
	eventID := uuid.New()
	occurredAt := time.Date(2024, 3, 1, 12, 34, 56, 987654321, time.UTC)

	env := cqrs.Envelope{
		EventID:    eventID,
		StreamID:   "order-1",
		Event:      want,
		Version:    1,
		OccurredAt: occurredAt,
		Metadata: map[string]any{
			"user":    "alice",
			"retries": 3,
			"trace":   map[string]any{"span": "abc"},
		},
	}

	if _, err := store.Save(ctx, []cqrs.Envelope{env}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectEnvelopes(t, ctx, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}
	got := loaded[0]

	gotEvent, ok := got.Event.(*roundtripEvent)
	if !ok {
		t.Fatalf("loaded event is %T, want *roundtripEvent", got.Event)
	}
	if !reflect.DeepEqual(gotEvent, want) {
		t.Fatalf("event changed on the way through disk:\n got: %#v\nwant: %#v", gotEvent, want)
	}

	// The pieces DeepEqual would also pass on if the whole substructure went
	// missing, called out so a regression names itself.
	if gotEvent.TraceID != want.TraceID {
		t.Errorf("TraceID = %v, want %v", gotEvent.TraceID, want.TraceID)
	}
	if !gotEvent.PlacedAt.Equal(want.PlacedAt) || gotEvent.PlacedAt.Nanosecond() != 123456789 {
		t.Errorf("PlacedAt = %v, want %v with nanoseconds intact", gotEvent.PlacedAt, want.PlacedAt)
	}
	if gotEvent.BillTo != nil {
		t.Errorf("BillTo = %#v, want a nil pointer to survive as nil", gotEvent.BillTo)
	}

	if got.EventID != eventID {
		t.Errorf("EventID = %v, want %v", got.EventID, eventID)
	}
	if got.StreamID != "order-1" {
		t.Errorf("StreamID = %q, want %q", got.StreamID, "order-1")
	}
	if got.Version != 1 {
		t.Errorf("Version = %d, want 1", got.Version)
	}
	if !got.OccurredAt.Equal(occurredAt) || got.OccurredAt.Nanosecond() != 987654321 {
		t.Errorf("OccurredAt = %v, want %v with nanoseconds intact", got.OccurredAt, occurredAt)
	}

	// Metadata is a map[string]any, so JSON — not the caller — decides the Go
	// type of every value: numbers always come back as float64.
	if got.Metadata["user"] != "alice" {
		t.Errorf(`Metadata["user"] = %#v, want "alice"`, got.Metadata["user"])
	}
	if v, ok := got.Metadata["retries"].(float64); !ok || v != 3 {
		t.Errorf(`Metadata["retries"] = %#v (%[1]T), want float64(3)`, got.Metadata["retries"])
	}
	trace, ok := got.Metadata["trace"].(map[string]any)
	if !ok {
		t.Fatalf(`Metadata["trace"] = %#v (%[1]T), want map[string]any`, got.Metadata["trace"])
	}
	if trace["span"] != "abc" {
		t.Errorf(`Metadata["trace"]["span"] = %#v, want "abc"`, trace["span"])
	}
}

// TestSerializationRoundtrip_BatchKeepsEventsDistinct asserts that each event
// in a multi-event batch is decoded into its own instance: a factory that
// returned a shared value, or a decode that reused one target, would leave
// every loaded event holding the last event's payload.
func TestSerializationRoundtrip_BatchKeepsEventsDistinct(t *testing.T) {
	ctx := context.Background()

	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	var envs []cqrs.Envelope
	for i := range 3 {
		ev := newRoundtripEvent("order-2")
		ev.Total = money{Amount: int64(100 * (i + 1)), Currency: "EUR"}
		ev.Labels = map[string]string{"seq": string(rune('a' + i))}
		envs = append(envs, cqrs.Envelope{
			EventID:    uuid.New(),
			StreamID:   "order-2",
			Event:      ev,
			Version:    uint64(i + 1),
			OccurredAt: time.Date(2024, 3, 1, 12, 0, i, 0, time.UTC),
			Metadata:   map[string]any{"seq": i},
		})
	}

	if _, err := store.Save(ctx, envs, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	iter, err := store.LoadStream(ctx, "order-2")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectEnvelopes(t, ctx, iter)
	if len(loaded) != len(envs) {
		t.Fatalf("LoadStream returned %d events, want %d", len(loaded), len(envs))
	}

	for i, got := range loaded {
		want := envs[i].Event.(*roundtripEvent)
		gotEvent, ok := got.Event.(*roundtripEvent)
		if !ok {
			t.Fatalf("loaded[%d] is %T, want *roundtripEvent", i, got.Event)
		}
		if !reflect.DeepEqual(gotEvent, want) {
			t.Errorf("loaded[%d] = %#v, want %#v", i, gotEvent, want)
		}
		if got.EventID != envs[i].EventID {
			t.Errorf("loaded[%d].EventID = %v, want %v", i, got.EventID, envs[i].EventID)
		}
		if got.Version != envs[i].Version {
			t.Errorf("loaded[%d].Version = %d, want %d", i, got.Version, envs[i].Version)
		}
		if v, ok := got.Metadata["seq"].(float64); !ok || int(v) != i {
			t.Errorf(`loaded[%d].Metadata["seq"] = %#v, want float64(%d)`, i, got.Metadata["seq"], i)
		}
	}
}

// TestSerializationRoundtrip_UnknownFieldOnDisk asserts that an event written
// by an older build — carrying a field the current struct no longer has —
// still loads, so an additive schema change does not break replay of streams
// that are already on disk.
func TestSerializationRoundtrip_UnknownFieldOnDisk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer store.Close()

	if _, err := store.Save(ctx, []cqrs.Envelope{{
		EventID:  uuid.New(),
		StreamID: "order-3",
		Event:    newRoundtripEvent("order-3"),
		Version:  1,
	}}, cqrs.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Rewrite the persisted payload the way an older build would have left
	// it: an extra field that no longer exists on roundtripEvent.
	rewritePayload(t, dir, "order-3", `{"order_id":"order-3","discount":7.5,"legacy_reason":"promo","total":{"amount":10,"currency":"EUR","vat":21}}`)

	iter, err := store.LoadStream(ctx, "order-3")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	loaded := collectEnvelopes(t, ctx, iter)
	if len(loaded) != 1 {
		t.Fatalf("LoadStream returned %d events, want 1", len(loaded))
	}

	got, ok := loaded[0].Event.(*roundtripEvent)
	if !ok {
		t.Fatalf("loaded event is %T, want *roundtripEvent", loaded[0].Event)
	}
	if got.OrderID != "order-3" {
		t.Errorf("OrderID = %q, want %q", got.OrderID, "order-3")
	}
	if got.Discount != 7.5 {
		t.Errorf("Discount = %v, want 7.5", got.Discount)
	}
	if (got.Total != money{Amount: 10, Currency: "EUR"}) {
		t.Errorf("Total = %#v, want money{10, EUR}", got.Total)
	}
	// Fields absent from the older payload stay at their zero value.
	if got.Items != nil {
		t.Errorf("Items = %#v, want nil for a field the stored payload never had", got.Items)
	}
	if !got.PlacedAt.IsZero() {
		t.Errorf("PlacedAt = %v, want the zero time", got.PlacedAt)
	}
}

func collectEnvelopes(t *testing.T, ctx context.Context, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()

	var out []*cqrs.Envelope
	for iter.Next(ctx) {
		out = append(out, iter.Value())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// rewritePayload replaces the "data" member of the single event file in
// streamID's directory, standing in for an event that an older build of the
// application wrote with a different payload shape.
func rewritePayload(t *testing.T, baseDir, streamID, payload string) {
	t.Helper()

	dir := filepath.Join(baseDir, streamsDirName, streamID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read stream dir %s: %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("stream %q holds %d files, want exactly 1", streamID, len(entries))
	}

	path := filepath.Join(dir, entries[0].Name())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored event %s: %v", path, err)
	}
	stored["data"] = json.RawMessage(payload)

	rewritten, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("marshal rewritten event: %v", err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
