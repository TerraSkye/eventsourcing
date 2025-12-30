package disk

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

type FilesStore struct {
	baseDir   string
	mu        sync.Mutex
	bus       chan *cqrs.Envelope
	globalSeq uint64
}

func NewFileStore(dir string) (*FilesStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "all"), 0o755); err != nil {
		return nil, err
	}
	return &FilesStore{
		baseDir: dir,
		bus:     make(chan *cqrs.Envelope, 100),
	}, nil
}

func (f *FilesStore) streamDir(id string) string {
	return filepath.Join(f.baseDir, id)
}

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

		eventData, _ := json.Marshal(events[i].Event)

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

		serialzedData, _ := json.Marshal(z)

		if err := os.WriteFile(path, serialzedData, 0o644); err != nil {
			return cqrs.AppendResult{StreamID: streamID, Successful: false}, err
		}
		// symlink to all/
		all := filepath.Join(f.baseDir, "all", fmt.Sprintf("%010d-%s.json", events[i].GlobalVersion, events[i].Event.EventType()))

		rel, _ := filepath.Rel(filepath.Join(f.baseDir, "all"), path)

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
	}

	return cqrs.AppendResult{
		StreamID:            streamID,
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil

}

func (f *FilesStore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), cqrs.StreamExists{})
}

func (f *FilesStore) LoadStreamFrom(ctx context.Context, id string, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), version)
}

func (f *FilesStore) LoadFromAll(ctx context.Context, version cqrs.StreamState) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(filepath.Join(f.baseDir, "all"), version)
}

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

func (f *FilesStore) Events() <-chan *cqrs.Envelope {
	return f.bus
}

func (f *FilesStore) Close() error {
	close(f.bus)
	return nil
}

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
