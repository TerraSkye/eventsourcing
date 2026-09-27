package file

import (
	"context"
	"os"
	"testing"

	cqrs "github.com/terraskye/eventsourcing"
)

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
