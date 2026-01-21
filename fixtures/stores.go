package fixtures

import (
	"context"
	"io"
	"sync"

	es "github.com/terraskye/eventsourcing"
)

// StoreSpy is a configurable mock EventStore for testing.
// It tracks calls and allows injecting custom behavior or failures.
type StoreSpy struct {
	mu sync.Mutex

	// Function overrides for custom behavior
	LoadStreamFn     func(ctx context.Context, id string) (*es.Iterator[*es.Envelope], error)
	LoadStreamFromFn func(ctx context.Context, id string, version es.StreamState) (*es.Iterator[*es.Envelope], error)
	LoadFromAllFn    func(ctx context.Context, version es.StreamState) (*es.Iterator[*es.Envelope], error)
	SaveFn           func(ctx context.Context, events []es.Envelope, revision es.StreamState) (es.AppendResult, error)
	CloseFn          func() error

	// Call tracking
	LoadStreamCalls     int
	LoadStreamFromCalls int
	LoadFromAllCalls    int
	SaveCalls           int
	CloseCalls          int

	// Captured arguments from last call
	LastSaveEvents   []es.Envelope
	LastSaveRevision es.StreamState
	LastLoadStreamID string

	// Pre-configured data
	events map[string][]*es.Envelope // streamID -> envelopes

	// Error injection
	loadErr error
	saveErr error
}

// NewStoreSpy creates a new StoreSpy with default behavior.
func NewStoreSpy() *StoreSpy {
	return &StoreSpy{
		events: make(map[string][]*es.Envelope),
	}
}

// WithEvents pre-populates the store with events for a stream.
func (s *StoreSpy) WithEvents(streamID string, events ...*es.Envelope) *StoreSpy {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events[streamID] = events
	return s
}

// WithEventsFromSlice pre-populates the store with events from Event slice.
func (s *StoreSpy) WithEventsFromSlice(streamID string, events ...es.Event) *StoreSpy {
	envelopes := EnvelopesFromEvents(events...)
	return s.WithEvents(streamID, envelopes...)
}

// FailOnLoad configures the store to return an error on load operations.
func (s *StoreSpy) FailOnLoad(err error) *StoreSpy {
	s.loadErr = err
	return s
}

// FailOnSave configures the store to return an error on save operations.
func (s *StoreSpy) FailOnSave(err error) *StoreSpy {
	s.saveErr = err
	return s
}

// LoadStream implements EventStore.LoadStream.
func (s *StoreSpy) LoadStream(ctx context.Context, id string) (*es.Iterator[*es.Envelope], error) {
	s.mu.Lock()
	s.LoadStreamCalls++
	s.LastLoadStreamID = id
	s.mu.Unlock()

	if s.LoadStreamFn != nil {
		return s.LoadStreamFn(ctx, id)
	}

	if s.loadErr != nil {
		return nil, s.loadErr
	}

	s.mu.Lock()
	events := s.events[id]
	s.mu.Unlock()

	return SliceIterator(events), nil
}

// LoadStreamFrom implements EventStore.LoadStreamFrom.
func (s *StoreSpy) LoadStreamFrom(ctx context.Context, id string, version es.StreamState) (*es.Iterator[*es.Envelope], error) {
	s.mu.Lock()
	s.LoadStreamFromCalls++
	s.LastLoadStreamID = id
	s.mu.Unlock()

	if s.LoadStreamFromFn != nil {
		return s.LoadStreamFromFn(ctx, id, version)
	}

	if s.loadErr != nil {
		return nil, s.loadErr
	}

	s.mu.Lock()
	events := s.events[id]
	s.mu.Unlock()

	// Filter events based on version
	fromVersion := uint64(0)
	if rev, ok := version.(es.Revision); ok {
		fromVersion = uint64(rev)
	}

	var filtered []*es.Envelope
	for _, e := range events {
		if e.Version > fromVersion {
			filtered = append(filtered, e)
		}
	}

	return SliceIterator(filtered), nil
}

// LoadFromAll implements EventStore.LoadFromAll.
func (s *StoreSpy) LoadFromAll(ctx context.Context, version es.StreamState) (*es.Iterator[*es.Envelope], error) {
	s.mu.Lock()
	s.LoadFromAllCalls++
	s.mu.Unlock()

	if s.LoadFromAllFn != nil {
		return s.LoadFromAllFn(ctx, version)
	}

	if s.loadErr != nil {
		return nil, s.loadErr
	}

	// Collect all events across streams
	s.mu.Lock()
	var all []*es.Envelope
	for _, events := range s.events {
		all = append(all, events...)
	}
	s.mu.Unlock()

	return SliceIterator(all), nil
}

// Save implements EventStore.Save.
func (s *StoreSpy) Save(ctx context.Context, events []es.Envelope, revision es.StreamState) (es.AppendResult, error) {
	s.mu.Lock()
	s.SaveCalls++
	s.LastSaveEvents = events
	s.LastSaveRevision = revision
	s.mu.Unlock()

	if s.SaveFn != nil {
		return s.SaveFn(ctx, events, revision)
	}

	if s.saveErr != nil {
		return es.AppendResult{Successful: false}, s.saveErr
	}

	if len(events) == 0 {
		return es.AppendResult{Successful: true}, nil
	}

	streamID := events[0].StreamID
	nextVersion := uint64(len(events))

	// Store the events
	s.mu.Lock()
	for i := range events {
		env := events[i]
		s.events[streamID] = append(s.events[streamID], &env)
	}
	s.mu.Unlock()

	return es.AppendResult{
		Successful:          true,
		StreamID:            streamID,
		NextExpectedVersion: nextVersion,
	}, nil
}

// Close implements EventStore.Close.
func (s *StoreSpy) Close() error {
	s.mu.Lock()
	s.CloseCalls++
	s.mu.Unlock()

	if s.CloseFn != nil {
		return s.CloseFn()
	}
	return nil
}

// Reset clears all call counts and stored data.
func (s *StoreSpy) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.LoadStreamCalls = 0
	s.LoadStreamFromCalls = 0
	s.LoadFromAllCalls = 0
	s.SaveCalls = 0
	s.CloseCalls = 0
	s.LastSaveEvents = nil
	s.LastSaveRevision = nil
	s.LastLoadStreamID = ""
	s.events = make(map[string][]*es.Envelope)
	s.loadErr = nil
	s.saveErr = nil
}

// Pre-built store scenarios.

// EmptyStore returns a StoreSpy with no events.
func EmptyStore() *StoreSpy {
	return NewStoreSpy()
}

// StoreWithEvents returns a StoreSpy pre-populated with n test events.
func StoreWithEvents(streamID string, n int) *StoreSpy {
	events := NewTestEvent().WithID(streamID).BuildN(n)
	return NewStoreSpy().WithEventsFromSlice(streamID, events...)
}

// FailingStore returns a StoreSpy that fails on all operations.
func FailingStore(err error) *StoreSpy {
	return NewStoreSpy().FailOnLoad(err).FailOnSave(err)
}

// ConcurrencyConflictStore returns a StoreSpy that returns a concurrency conflict on save.
func ConcurrencyConflictStore(streamID string, expected, actual es.StreamState) *StoreSpy {
	store := NewStoreSpy()
	store.SaveFn = func(ctx context.Context, events []es.Envelope, revision es.StreamState) (es.AppendResult, error) {
		return es.AppendResult{Successful: false}, es.StreamRevisionConflictError{
			Stream:           streamID,
			ExpectedRevision: expected,
			ActualRevision:   actual,
		}
	}
	return store
}

// StreamNotFoundStore returns a StoreSpy that returns ErrStreamNotFound on load.
func StreamNotFoundStore() *StoreSpy {
	store := NewStoreSpy()
	store.LoadStreamFn = func(ctx context.Context, id string) (*es.Iterator[*es.Envelope], error) {
		return nil, es.ErrStreamNotFound
	}
	store.LoadStreamFromFn = func(ctx context.Context, id string, version es.StreamState) (*es.Iterator[*es.Envelope], error) {
		return nil, es.ErrStreamNotFound
	}
	return store
}

// SliceIterator creates an iterator from a slice of envelope pointers.
func SliceIterator(envelopes []*es.Envelope) *es.Iterator[*es.Envelope] {
	idx := 0
	return es.NewIteratorFunc(func(ctx context.Context) (*es.Envelope, error) {
		if idx >= len(envelopes) {
			return nil, io.EOF
		}
		env := envelopes[idx]
		idx++
		return env, nil
	})
}
