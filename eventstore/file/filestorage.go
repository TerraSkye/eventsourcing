package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
)

var _ cqrs.EventStore = (*FilesStore)(nil)

// allDirName is the top-level directory holding the symlink fan-in of every
// event across all streams, used by [FilesStore.LoadFromAll].
const allDirName = "all"

// streamsDirName is the top-level directory under which each stream gets
// its own subdirectory, named streamsDirName/<streamID>. Nesting streams
// one level down, rather than storing streamID directly under baseDir,
// keeps any stream ID — including "all" itself — from ever colliding with
// allDirName.
const streamsDirName = "streams"

// FilesStore is a file-backed [cqrs.EventStore] intended for tests and local
// development. Each stream's events are stored as one JSON file per event
// under its own directory (see streamDir), and a symlink to every event is
// also kept under an "all" directory to support [FilesStore.LoadFromAll].
// It is safe for concurrent use.
type FilesStore struct {
	baseDir   string
	mu        sync.Mutex
	closed    bool
	bus       chan *cqrs.Envelope
	globalSeq uint64
	watcher   *fsnotify.Watcher
	watchDone chan struct{}
}

// NewFileStore creates a [FilesStore] rooted at dir, creating dir and its
// reserved subdirectories if they do not already exist. If dir already
// contains events from a previous instance, the global sequence used to
// number new events (see Save) resumes after the highest one already on
// disk, rather than restarting at zero.
//
// A background watcher then keeps that sequence in sync with allDir for the
// life of the store (see watchGlobalSequence), not just at startup: if
// another FilesStore instance is writing into the same directory
// concurrently, its symlinks bump this store's globalSeq too, so the two
// don't reissue each other's versions.
func NewFileStore(dir string) (*FilesStore, error) {
	allDir := filepath.Join(dir, allDirName)
	if err := os.MkdirAll(allDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, streamsDirName), 0o755); err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create watcher: %w", err)
	}

	// Attached before the recovery listing below, so any symlink a
	// concurrent writer completes during that listing is queued rather
	// than missed — watchGlobalSequence picks it up once it starts,
	// exactly like every later write.
	if err := watcher.Add(allDir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("watch %s: %w", allDir, err)
	}

	globalSeq, err := highestGlobalVersion(allDir)
	if err != nil {
		watcher.Close()
		return nil, fmt.Errorf("recover global sequence: %w", err)
	}

	f := &FilesStore{
		baseDir:   dir,
		bus:       make(chan *cqrs.Envelope, 100),
		globalSeq: globalSeq,
		watcher:   watcher,
		watchDone: make(chan struct{}),
	}

	go f.watchGlobalSequence()

	return f, nil
}

// highestGlobalVersion returns the highest global version already recorded
// in allDir (named "%010d-<EventType>.json"; see Save), or 0 if allDir is
// empty.
func highestGlobalVersion(allDir string) (uint64, error) {
	entries, err := os.ReadDir(allDir)
	if err != nil {
		return 0, err
	}

	var highest uint64
	for _, e := range entries {
		if v, ok := parseGlobalVersion(e.Name()); ok && v > highest {
			highest = v
		}
	}
	return highest, nil
}

// watchGlobalSequence keeps f.globalSeq in sync with allDir for the life of
// the store: whenever a new symlink appears there — from this store's own
// Save or from a peer FilesStore instance writing into the same directory
// concurrently — its encoded global version is folded into f.globalSeq if
// higher, so the next Save (by either instance, once it also observes the
// other's writes) resumes past it instead of reissuing it. It returns once
// f.watcher is closed (see Close).
func (f *FilesStore) watchGlobalSequence() {
	defer close(f.watchDone)

	for {
		select {
		case ev, ok := <-f.watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			v, ok := parseGlobalVersion(filepath.Base(ev.Name))
			if !ok {
				continue
			}
			f.mu.Lock()
			if v > f.globalSeq {
				f.globalSeq = v
			}
			f.mu.Unlock()

		case _, ok := <-f.watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// parseGlobalVersion extracts the global version encoded in a filename of
// the form "%010d-<EventType>.json" (see Save), reporting ok=false if name
// doesn't match that pattern.
func parseGlobalVersion(name string) (uint64, bool) {
	name, ok := strings.CutSuffix(name, ".json")
	if !ok {
		return 0, false
	}
	numPart, _, ok := strings.Cut(name, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(numPart, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (f *FilesStore) streamDir(id string) string {
	return filepath.Join(f.baseDir, streamsDirName, id)
}

// Save appends events to the stream they share, enforcing the concurrency
// check described by revision: [cqrs.Any] skips it, [cqrs.NoStream] requires
// the stream not to exist yet, [cqrs.StreamExists] requires that it already
// does, and a [cqrs.Revision] requires the stream's current length to match
// exactly, returning a [cqrs.StreamRevisionConflictError] on mismatch. Save
// also returns a [cqrs.StreamRevisionConflictError] if the global version it
// assigns an event collides with one a concurrent writer already claimed
// (see watchGlobalSequence) — its ActualRevision is then the colliding
// global version rather than a stream length, but the reaction is the same
// either way: retry the whole Save. All events must have the same
// StreamID, or Save returns a non-nil error without appending anything.
//
// events is written as a single unit: if any event fails to marshal or
// write, or its global version collides, every file and symlink already
// written for this call is removed before Save returns, rather than left
// as a partial batch on disk. Only once every event has been durably
// written does Save publish them on Events, so a subscriber never observes
// part of a batch that later failed. On success it returns a
// [cqrs.AppendResult] whose NextExpectedVersion is the stream's new length.
//
// TODO: each event is written to a file named after its own Version field
// (e.g. "0000000002-Foo.json"), but Save never assigns that field itself —
// unlike the postgres and kurrentdb implementations of this interface, which
// compute the position server-side. If the caller does not set Version to a
// value that is unique and sequential within the stream (for example, if it
// is left at its zero value across a multi-event batch), later events
// silently overwrite earlier ones on disk and Save still reports success
// with no error (confirmed: saving 3 events with Version left unset leaves
// only 1 file, and LoadStream then returns only that 1 event).
func (f *FilesStore) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.StreamState) (cqrs.AppendResult, error) {
	if len(events) == 0 {
		return cqrs.AppendResult{Successful: true}, nil
	}

	var streamID = events[0].StreamID
	sdir := f.streamDir(streamID)

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return cqrs.AppendResult{Successful: false, StreamID: streamID}, fmt.Errorf("save to stream %q: event store is closed", streamID)
	}

	os.MkdirAll(sdir, 0o755)

	// Determine current version
	files, _ := os.ReadDir(sdir)

	currentVersion := uint64(len(files))

	switch rev := revision.(type) {
	case cqrs.Any:
		// No concurrency check
	case cqrs.NoStream:
		if currentVersion != 0 {
			err := fmt.Errorf("stream already exists for stream %s", streamID)
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, err
		}
	case cqrs.StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream does not exist for stream %s", streamID)
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, err
		}
	case cqrs.Revision:
		if currentVersion != uint64(rev) {
			return cqrs.AppendResult{Successful: false, StreamID: streamID},
				&cqrs.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: rev,
					ActualRevision:   cqrs.Revision(currentVersion),
				}
		}
	default:
		err := fmt.Errorf("unsupported revision type for stream %s", streamID)
		return cqrs.AppendResult{Successful: false, StreamID: streamID}, err
	}

	// written accumulates every path created for this call, in creation
	// order, so a failure partway through can undo exactly what this call
	// itself wrote — f.globalSeq is deliberately not rolled back alongside
	// it: those global versions were momentarily visible on disk, so a peer
	// instance's watcher may already have observed them, and reissuing them
	// on the next attempt would recreate the same conflict.
	written := make([]string, 0, len(events)*2)
	rollback := func() {
		for i := len(written) - 1; i >= 0; i-- {
			_ = os.Remove(written[i])
		}
	}

	// Append events
	for i := range events {
		select {
		case <-ctx.Done():
			rollback()
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, ctx.Err()
		default:
		}
		f.globalSeq++
		events[i].GlobalVersion = f.globalSeq

		fname := fmt.Sprintf("%010d-%s.json", events[i].Version, events[i].Event.EventType())

		path := filepath.Join(sdir, fname)

		eventData, err := json.Marshal(events[i].Event)
		if err != nil {
			rollback()
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, fmt.Errorf("marshal event data: %w", err)
		}

		z := storedEvent{
			EventID:       events[i].EventID,
			StreamID:      events[i].StreamID,
			Metadata:      events[i].Metadata,
			EventType:     events[i].Event.EventType(),
			Data:          eventData,
			Version:       events[i].Version,
			GlobalVersion: events[i].GlobalVersion,
			OccurredAt:    events[i].OccurredAt,
		}

		serializedData, err := json.Marshal(z)
		if err != nil {
			rollback()
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, fmt.Errorf("marshal event: %w", err)
		}

		if err := os.WriteFile(path, serializedData, 0o644); err != nil {
			rollback()
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, err
		}
		written = append(written, path)

		// symlink to all/
		all := filepath.Join(f.baseDir, allDirName, fmt.Sprintf("%010d-%s.json", events[i].GlobalVersion, events[i].Event.EventType()))

		rel, _ := filepath.Rel(filepath.Join(f.baseDir, allDirName), path)

		if err := os.Symlink(rel, all); err != nil {
			rollback()
			if errors.Is(err, fs.ErrExist) {
				return cqrs.AppendResult{StreamID: streamID, Successful: false}, &cqrs.StreamRevisionConflictError{
					Stream:           streamID,
					ExpectedRevision: revision,
					ActualRevision:   cqrs.Revision(events[i].GlobalVersion),
				}
			}
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, err
		}
		written = append(written, all)

		currentVersion++
	}

	// Only publish once the whole batch is durably on disk, so a
	// subscriber never sees part of a batch that a later event in it then
	// failed and rolled back.
	for i := range events {
		select {
		case f.bus <- &events[i]:
		default:
			// Drop event if channel full
		}
	}

	return cqrs.AppendResult{
		StreamID:            streamID,
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil

}

// LoadStream returns a lazy iterator over all events in the stream
// identified by id, in the order they were appended. It returns a non-nil
// error if the stream does not exist.
func (f *FilesStore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), cqrs.StreamExists{}, false)
}

// LoadStreamFrom returns a lazy iterator over the events in the stream
// identified by id, starting at the position identified by version. A
// [cqrs.Revision] starts at that index; [cqrs.NoStream] requires the stream
// not to exist and returns a non-nil error if it does; [cqrs.StreamExists]
// requires that it does exist and returns a non-nil error if it does not;
// any other [cqrs.StreamState], including [cqrs.Any], reads from the
// beginning of the stream. It also returns a non-nil error if the requested
// revision is beyond the stream's current length.
func (f *FilesStore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), version, false)
}

// LoadFromAll returns a lazy iterator over every event saved across all
// streams, ordered by the [cqrs.Envelope] GlobalVersion assigned when each
// event was appended. version is interpreted the same way as in
// [FilesStore.LoadStreamFrom], but against the global sequence rather than a
// single stream.
func (f *FilesStore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(filepath.Join(f.baseDir, allDirName), version, true)
}

// loadFromDir is the shared implementation behind LoadStream, LoadStreamFrom,
// and LoadFromAll: it lists dir, applies the precondition or starting offset
// described by from, and returns a lazy iterator over the decoded events in
// filename order.
//
// oneIndexed distinguishes the two numbering conventions files in dir are
// named by: a stream directory's files are named after Envelope.Version,
// which starts at 0, while the "all" directory's files are named after
// Envelope.GlobalVersion, which starts at 1 — so a Revision(N) start
// position (meaning "N events already seen") must skip versions < N in the
// 0-indexed case, but versions <= N in the 1-indexed one.
func (f *FilesStore) loadFromDir(dir string, from cqrs.StreamState, oneIndexed bool) (*cqrs.Iterator[*cqrs.Envelope], error) {
	// A stream's directory is only created lazily, on its first successful
	// Save, so a never-saved stream (a perfectly normal thing to load, e.g.
	// via Any{} or NoStream{} for a brand-new aggregate) has no directory at
	// all yet — that's an empty stream, not an error, and the StreamState
	// switch below decides whether an empty stream is acceptable.
	files, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		files = nil
	}

	var offset uint64

	switch from.(type) {
	case cqrs.NoStream:
		if len(files) != 0 {
			return nil, fmt.Errorf(
				"load stream %q: expected empty stream: %w",
				dir, cqrs.ErrStreamExists,
			)
		}
	case cqrs.StreamExists:
		if len(files) == 0 {
			return nil, fmt.Errorf(
				"load stream %q: expected existing stream: %w",
				dir, cqrs.ErrStreamNotFound,
			)
		}
	case cqrs.Revision:
		// A start index equal to the stream's length is valid — it means
		// "give me anything newer than what I've already seen", the same
		// half-open convention Save itself relies on when it accepts
		// Revision(currentVersion) as the expected revision to append
		// against. Only a position beyond the stream's length is invalid.
		if int(from.ToRawInt64()) > len(files) {
			return nil, fmt.Errorf(
				"load stream %q: requested %d but stream has %d: %w",
				dir, from, len(files), cqrs.ErrInvalidRevision,
			)
		}
		offset = uint64(from.ToRawInt64())
	default:
	}

	idx := 0
	nextFunc := func(ctx context.Context) (*cqrs.Envelope, error) {
		for idx < len(files) {
			fi := files[idx]
			idx++
			if fi.IsDir() {
				continue
			}

			parts := strings.Split(fi.Name(), "-")
			if len(parts) < 2 {
				continue
			}
			ver, _ := strconv.ParseUint(parts[0], 10, 64)
			if oneIndexed {
				if ver <= offset {
					continue
				}
			} else if ver < offset {
				continue
			}

			path := filepath.Join(dir, fi.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}

			var storedEv storedEvent
			if err := json.Unmarshal(data, &storedEv); err != nil {
				continue
			}

			// Convert KurrentDB event to cqrs.EventData
			ev, err := cqrs.NewEventByName(storedEv.EventType)
			if err != nil {
				// Wrap and propagate as EventStoreError
				return nil, fmt.Errorf("cannot create event %q: %w", storedEv.EventType, err)
			}

			if err := json.Unmarshal(storedEv.Data, &ev); err != nil {
				return nil, fmt.Errorf("cannot unmarshal event %q: %w", storedEv.EventType, err)
			}

			envelope := cqrs.Envelope{
				EventID:       storedEv.EventID,
				StreamID:      storedEv.StreamID,
				Event:         ev,
				Metadata:      storedEv.Metadata,
				Version:       storedEv.Version,
				GlobalVersion: storedEv.GlobalVersion,
				OccurredAt:    storedEv.OccurredAt,
			}

			return &envelope, nil
		}
		return nil, io.EOF
	}

	return cqrs.NewIteratorFunc(nextFunc), nil
}

// Events returns a channel that receives every [cqrs.Envelope] as it is
// saved. It is not part of the [cqrs.EventStore] interface; it exists so
// tests and local tooling can observe writes as they happen. The channel has
// a fixed buffer of 100 and sends are non-blocking, so a slow consumer
// misses events rather than blocking Save. The channel is closed by Close.
func (f *FilesStore) Events() <-chan *cqrs.Envelope {
	return f.bus
}

// Close stops the background watcher (see watchGlobalSequence) and closes
// the channel returned by Events. After Close, Save returns an error
// instead of appending. Calling Close more than once is a no-op.
func (f *FilesStore) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()

	// Closing the watcher unblocks watchGlobalSequence's read of
	// f.watcher.Events; wait for it to return before closing f.bus so no
	// goroutine is left running past Close. f.mu must not be held across
	// this wait: watchGlobalSequence itself needs to acquire it to apply
	// any event still in flight when Close was called.
	f.watcher.Close()
	<-f.watchDone

	close(f.bus)
	return nil
}

// storedEvent is the on-disk JSON representation of a saved [cqrs.Envelope].
type storedEvent struct {
	EventID       uuid.UUID       `json:"event_id"`
	StreamID      string          `json:"stream_id"`
	Metadata      map[string]any  `json:"metadata"`
	EventType     string          `json:"event_type"`
	Data          json.RawMessage `json:"data"`
	Version       uint64          `json:"version"`
	GlobalVersion uint64          `json:"global_version"`
	OccurredAt    time.Time       `json:"occurred_at"`
}
