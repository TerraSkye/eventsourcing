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

func (f *FilesStore) Save(ctx context.Context, events []cqrs.Envelope, revision cqrs.Revision) (cqrs.AppendResult, error) {
	if len(events) == 0 {
		return cqrs.AppendResult{Successful: true}, nil
	}

	id := events[0].StreamID
	sdir := f.streamDir(id)

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
			err := fmt.Errorf("stream already exists for stream %s", id)
			return cqrs.AppendResult{Successful: false}, err
		}
	case cqrs.StreamExists:
		if currentVersion == 0 {
			err := fmt.Errorf("stream does not exist for stream %s", id)
			return cqrs.AppendResult{Successful: false}, err
		}
	case cqrs.ExplicitRevision:
		if currentVersion != uint64(rev) {
			err := fmt.Errorf("version mismatch for stream %s: expected %d, got %d", id, rev, currentVersion)
			return cqrs.AppendResult{Successful: false}, err
		}
	default:
		err := fmt.Errorf("unsupported revision type for stream %s", id)
		return cqrs.AppendResult{Successful: false}, err
	}

	// Append events
	for i := range events {
		select {
		case <-ctx.Done():
			return cqrs.AppendResult{Successful: false}, ctx.Err()
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
			return cqrs.AppendResult{}, err
		}
		// symlink to all/
		all := filepath.Join(f.baseDir, "all", fmt.Sprintf("%010d-%s.json", events[i].GlobalVersion, events[i].Event.EventType()))

		rel, _ := filepath.Rel(filepath.Join(f.baseDir, "all"), path)

		if err := os.Symlink(rel, all); err != nil {
			return cqrs.AppendResult{}, err
		}

		select {
		case f.bus <- &events[i]:
		default:
			// Drop error if channel full
		}
	}

	return cqrs.AppendResult{
		Successful:          true,
		NextExpectedVersion: currentVersion,
	}, nil

}

func (f *FilesStore) LoadStream(ctx context.Context, id string) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), 0)
}

func (f *FilesStore) LoadStreamFrom(ctx context.Context, id string, version uint64) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(f.streamDir(id), version)
}

func (f *FilesStore) loadFromDir(dir string, from uint64) (*cqrs.Iterator[*cqrs.Envelope], error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return cqrs.NewIterator(func(ctx context.Context) (*cqrs.Envelope, error) {
				return nil, io.EOF
			}), nil
		}
		return nil, err
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
			if ver < from {
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

			// Convert KurrentDB event to cqrs.Event
			ev, err := cqrs.NewEventByName(storedEv.EventType)
			if err != nil {
				// Wrap and propagate as EventStoreError
				return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot create event %q: %w", storedEv.EventType, err))
			}

			if err := json.Unmarshal(storedEv.Data, &ev); err != nil {
				return nil, cqrs.WrapEventStoreError(fmt.Errorf("cannot unmarshal event %q: %w", storedEv.EventType, err))
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

	return cqrs.NewIterator(nextFunc), nil
}

func (f *FilesStore) LoadFromAll(ctx context.Context, version uint64) (*cqrs.Iterator[*cqrs.Envelope], error) {
	return f.loadFromDir(filepath.Join(f.baseDir, "all"), version)
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
