package file

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
	bus       chan *cqrs.Envelope
	globalSeq uint64
}

// NewFileStore creates a [FilesStore] rooted at dir, creating dir and its
// reserved subdirectories if they do not already exist.
func NewFileStore(dir string) (*FilesStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, allDirName), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, streamsDirName), 0o755); err != nil {
		return nil, err
	}
	return &FilesStore{
		baseDir: dir,
		bus:     make(chan *cqrs.Envelope, 100),
	}, nil
}

func (f *FilesStore) streamDir(id string) string {
	return filepath.Join(f.baseDir, streamsDirName, id)
}

// Save appends events to the stream they share, enforcing the concurrency
// check described by revision: [cqrs.Any] skips it, [cqrs.NoStream] requires
// the stream not to exist yet, [cqrs.StreamExists] requires that it already
// does, and a [cqrs.Revision] requires the stream's current length to match
// exactly, returning a [cqrs.StreamRevisionConflictError] on mismatch. All
// events must have the same StreamID, or Save returns a non-nil error
// without appending anything. On success it returns a [cqrs.AppendResult]
// whose NextExpectedVersion is the stream's new length.
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

	// Append events
	for i := range events {
		select {
		case <-ctx.Done():
			return cqrs.AppendResult{Successful: false, StreamID: streamID}, ctx.Err()
		default:
		}
		f.globalSeq++
		events[i].GlobalVersion = f.globalSeq

		fname := fmt.Sprintf("%010d-%s.json", events[i].Version, events[i].Event.EventType())

		path := filepath.Join(sdir, fname)

		eventData, err := json.Marshal(events[i].Event)
		if err != nil {
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
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, fmt.Errorf("marshal event: %w", err)
		}

		if err := os.WriteFile(path, serializedData, 0o644); err != nil {
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, err
		}
		// symlink to all/
		all := filepath.Join(f.baseDir, allDirName, fmt.Sprintf("%010d-%s.json", events[i].GlobalVersion, events[i].Event.EventType()))

		rel, _ := filepath.Rel(filepath.Join(f.baseDir, allDirName), path)

		if err := os.Symlink(rel, all); err != nil {
			return cqrs.AppendResult{
				StreamID:   streamID,
				Successful: false,
			}, err
		}

		select {
		case f.bus <- &events[i]:
		default:
			// Drop error if channel full
		}

		currentVersion++
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
	return f.loadFromDir(f.streamDir(id), cqrs.StreamExists{})
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
	return f.loadFromDir(f.streamDir(id), version)
}

// LoadFromAll returns a lazy iterator over every event saved across all
// streams, ordered by the [cqrs.Envelope] GlobalVersion assigned when each
// event was appended. version is interpreted the same way as in
// [FilesStore.LoadStreamFrom], but against the global sequence rather than a
// single stream.
func (f *FilesStore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(filepath.Join(f.baseDir, allDirName), version)
}

// loadFromDir is the shared implementation behind LoadStream, LoadStreamFrom,
// and LoadFromAll: it lists dir, applies the precondition or starting offset
// described by from, and returns a lazy iterator over the decoded events in
// filename order.
func (f *FilesStore) loadFromDir(dir string, from cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		//TODO handle errors.
		return nil, err
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
		if int(from.ToRawInt64()) >= len(files) {
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
			if ver < offset {
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

// Close closes the channel returned by Events. After Close, the FilesStore
// must not be used again.
//
// TODO: this is not idempotent, contrary to the cqrs.EventStore contract,
// which asks implementations to make Close safe to call more than once — a
// second call panics with "close of closed channel".
func (f *FilesStore) Close() error {
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
