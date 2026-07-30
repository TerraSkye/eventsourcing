package memory_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventstore/memory"
)

// Test event types

type OrderCreated struct {
	OrderID    string
	CustomerID string
}

func (e OrderCreated) AggregateID() string { return e.OrderID }
func (e OrderCreated) EventType() string   { return "OrderCreated" }

type ItemAdded struct {
	OrderID string
	ItemID  string
	Qty     int
}

func (e ItemAdded) AggregateID() string { return e.OrderID }
func (e ItemAdded) EventType() string   { return "ItemAdded" }

type OrderShipped struct {
	OrderID string
}

func (e OrderShipped) AggregateID() string { return e.OrderID }
func (e OrderShipped) EventType() string   { return "OrderShipped" }

// Helper functions

func newEnvelope(streamID string, event cqrs.Event) cqrs.Envelope {
	return cqrs.Envelope{
		EventID:    uuid.New(),
		StreamID:   streamID,
		Event:      event,
		OccurredAt: time.Now(),
		Metadata:   map[string]any{},
	}
}

func collectAll(t *testing.T, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()
	ctx := context.Background()
	var results []*cqrs.Envelope
	for iter.Next(ctx) {
		results = append(results, iter.Value())
	}
	if err := iter.Err(); err != nil && err != io.EOF {
		t.Fatalf("iterator error: %v", err)
	}
	return results
}

// Save Tests

func TestSave_EmptySlice(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	result, err := store.Save(context.Background(), []cqrs.Envelope{}, cqrs.Any{})

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_SingleEvent(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	event := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	result, err := store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Any{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
	if result.StreamID != "order-1" {
		t.Errorf("expected StreamID 'order-1', got %q", result.StreamID)
	}
	if result.NextExpectedVersion != 1 {
		t.Errorf("expected NextExpectedVersion 1, got %d", result.NextExpectedVersion)
	}
}

func TestSave_MultipleEvents(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 2}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-2", Qty: 1}),
	}

	result, err := store.Save(context.Background(), events, cqrs.Any{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.NextExpectedVersion != 3 {
		t.Errorf("expected NextExpectedVersion 3, got %d", result.NextExpectedVersion)
	}
}

func TestSave_MixedStreamIDs_Fails(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-2", OrderCreated{OrderID: "order-2", CustomerID: "cust-2"}),
	}

	result, err := store.Save(context.Background(), events, cqrs.Any{})

	if err == nil {
		t.Fatal("expected error for mixed stream IDs")
	}
	if !errors.Is(err, cqrs.ErrInvalidEventBatch) {
		t.Errorf("expected ErrInvalidEventBatch, got %v", err)
	}
	if result.Successful {
		t.Error("expected unsuccessful result")
	}
}

// Revision Tests

func TestSave_NoStream_Success(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	event := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	result, err := store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.NoStream{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_NoStream_FailsWhenStreamExists(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Create stream first
	event1 := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []cqrs.Envelope{event1}, cqrs.Any{})

	// Try to save with NoStream
	event2 := newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1})
	_, err := store.Save(context.Background(), []cqrs.Envelope{event2}, cqrs.NoStream{})

	if err == nil {
		t.Fatal("expected error when stream already exists")
	}
	if !errors.Is(err, cqrs.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

func TestSave_StreamExists_Success(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Create stream first
	event1 := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []cqrs.Envelope{event1}, cqrs.Any{})

	// Append with StreamExists
	event2 := newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1})
	result, err := store.Save(context.Background(), []cqrs.Envelope{event2}, cqrs.StreamExists{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_StreamExists_FailsWhenNoStream(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	event := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, err := store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.StreamExists{})

	if err == nil {
		t.Fatal("expected error when stream doesn't exist")
	}
	if !errors.Is(err, cqrs.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestSave_Revision_Success(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Create stream with 2 events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Append at correct revision
	event := newEnvelope("order-1", OrderShipped{OrderID: "order-1"})
	result, err := store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Revision(2))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.NextExpectedVersion != 3 {
		t.Errorf("expected NextExpectedVersion 3, got %d", result.NextExpectedVersion)
	}
}

func TestSave_Revision_Conflict(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Create stream with 2 events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Try to append at wrong revision
	event := newEnvelope("order-1", OrderShipped{OrderID: "order-1"})
	_, err := store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Revision(1))

	if err == nil {
		t.Fatal("expected conflict error")
	}

	var conflictErr *cqrs.StreamRevisionConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected StreamRevisionConflictError, got %T: %v", err, err)
	}
}

// LoadStream Tests

func TestLoadStream_ExistingStream(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 2}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Load stream
	iter, err := store.LoadStream(context.Background(), "order-1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 2 {
		t.Errorf("expected 2 events, got %d", len(loaded))
	}

	if loaded[0].Event.EventType() != "OrderCreated" {
		t.Errorf("expected first event OrderCreated, got %s", loaded[0].Event.EventType())
	}
	if loaded[1].Event.EventType() != "ItemAdded" {
		t.Errorf("expected second event ItemAdded, got %s", loaded[1].Event.EventType())
	}
}

func TestLoadStream_NonExistingStream(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	_, err := store.LoadStream(context.Background(), "non-existing")

	if err == nil {
		t.Fatal("expected error for non-existing stream")
	}
	if !errors.Is(err, cqrs.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestLoadStream_ContextCancellation(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save many events
	events := make([]cqrs.Envelope, 100)
	for i := range events {
		events[i] = newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Load with cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Read a few events then cancel
	iter.Next(ctx)
	iter.Next(ctx)
	cancel()

	// Next call should detect cancellation
	if iter.Next(ctx) {
		// Context check happens at start of Next, so it should return false
		if iter.Err() != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", iter.Err())
		}
	}
}

// LoadStreamFrom Tests

func TestLoadStreamFrom_AtVersion(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save 5 events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-2", Qty: 2}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-3", Qty: 3}),
		newEnvelope("order-1", OrderShipped{OrderID: "order-1"}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Load from version 2 (0-indexed, so skip first 2)
	iter, err := store.LoadStreamFrom(context.Background(), "order-1", cqrs.Revision(2))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 3 {
		t.Errorf("expected 3 events, got %d", len(loaded))
	}

	// First event should be the third one (item-2)
	if itemAdded, ok := loaded[0].Event.(ItemAdded); !ok || itemAdded.ItemID != "item-2" {
		t.Errorf("expected ItemAdded with item-2, got %+v", loaded[0].Event)
	}
}

func TestLoadStreamFrom_InvalidVersion(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save 2 events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Try to load from version beyond stream length
	_, err := store.LoadStreamFrom(context.Background(), "order-1", cqrs.Revision(10))

	if err == nil {
		t.Fatal("expected error for invalid version")
	}
	if !errors.Is(err, cqrs.ErrInvalidRevision) {
		t.Errorf("expected ErrInvalidRevision, got %v", err)
	}
}

func TestLoadStreamFrom_StreamExists(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Load with StreamExists
	iter, err := store.LoadStreamFrom(context.Background(), "order-1", cqrs.StreamExists{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Errorf("expected 1 event, got %d", len(loaded))
	}
}

func TestLoadStreamFrom_NoStream(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save events
	events := []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}
	_, _ = store.Save(context.Background(), events, cqrs.Any{})

	// Load with NoStream should fail
	_, err := store.LoadStreamFrom(context.Background(), "order-1", cqrs.NoStream{})

	if err == nil {
		t.Fatal("expected error for NoStream on existing stream")
	}
	if !errors.Is(err, cqrs.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

// LoadFromAll Tests

func TestLoadFromAll_AllEvents(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save events to multiple streams
	_, _ = store.Save(context.Background(), []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}, cqrs.Any{})

	_, _ = store.Save(context.Background(), []cqrs.Envelope{
		newEnvelope("order-2", OrderCreated{OrderID: "order-2", CustomerID: "cust-2"}),
	}, cqrs.Any{})

	_, _ = store.Save(context.Background(), []cqrs.Envelope{
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}, cqrs.Any{})

	// Load all from beginning
	iter, err := store.LoadFromAll(context.Background(), cqrs.Revision(0))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 3 {
		t.Errorf("expected 3 events, got %d", len(loaded))
	}

	// Verify order is preserved (global order)
	if loaded[0].StreamID != "order-1" || loaded[0].Event.EventType() != "OrderCreated" {
		t.Errorf("first event mismatch: %+v", loaded[0])
	}
	if loaded[1].StreamID != "order-2" {
		t.Errorf("second event mismatch: %+v", loaded[1])
	}
	if loaded[2].StreamID != "order-1" || loaded[2].Event.EventType() != "ItemAdded" {
		t.Errorf("third event mismatch: %+v", loaded[2])
	}
}

func TestLoadFromAll_FromPosition(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save 5 events
	for i := 0; i < 5; i++ {
		_, _ = store.Save(context.Background(), []cqrs.Envelope{
			newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i}),
		}, cqrs.Any{})
	}

	// Load from position 2
	iter, err := store.LoadFromAll(context.Background(), cqrs.Revision(2))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 3 {
		t.Errorf("expected 3 events, got %d", len(loaded))
	}
}

func TestLoadFromAll_InvalidPosition(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	// Save 2 events
	_, _ = store.Save(context.Background(), []cqrs.Envelope{
		newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}, cqrs.Any{})

	// Try to load from position beyond total events
	_, err := store.LoadFromAll(context.Background(), cqrs.Revision(10))

	if err == nil {
		t.Fatal("expected error for invalid position")
	}
	if !errors.Is(err, cqrs.ErrInvalidRevision) {
		t.Errorf("expected ErrInvalidRevision, got %v", err)
	}
}

// Events Channel Tests

func TestEvents_ReceivesPublishedEvents(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	eventsChan := store.Events()

	// Save an event
	event := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Any{})

	select {
	case received := <-eventsChan:
		if received.Event.EventType() != "OrderCreated" {
			t.Errorf("expected OrderCreated, got %s", received.Event.EventType())
		}
	case <-time.After(time.Second):
		t.Error("timeout waiting for event")
	}
}

// Close Tests

func TestClose(t *testing.T) {
	store := memory.NewMemoryStore(10)

	err := store.Close()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	// Events channel should be closed
	eventsChan := store.Events()
	select {
	case _, ok := <-eventsChan:
		if ok {
			t.Error("expected events channel to be closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected events channel to be closed immediately")
	}
}

// Concurrency Tests

func TestConcurrent_Saves(t *testing.T) {
	store := memory.NewMemoryStore(100)
	defer store.Close()

	done := make(chan bool)
	numGoroutines := 10
	eventsPerGoroutine := 10

	for i := 0; i < numGoroutines; i++ {
		go func(streamNum int) {
			streamID := "order-" + string(rune('A'+streamNum))
			for j := 0; j < eventsPerGoroutine; j++ {
				event := newEnvelope(streamID, ItemAdded{OrderID: streamID, ItemID: "item", Qty: j})
				_, _ = store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Any{})
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify total events
	iter, err := store.LoadFromAll(context.Background(), cqrs.Revision(0))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	expected := numGoroutines * eventsPerGoroutine
	if len(loaded) != expected {
		t.Errorf("expected %d events, got %d", expected, len(loaded))
	}
}

func TestConcurrent_SaveAndLoad(t *testing.T) {
	store := memory.NewMemoryStore(100)
	defer store.Close()

	// First create the stream
	event := newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Any{})

	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 50; i++ {
			event := newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
			_, _ = store.Save(context.Background(), []cqrs.Envelope{event}, cqrs.Any{})
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 50; i++ {
			iter, err := store.LoadStream(context.Background(), "order-1")
			if err != nil {
				continue
			}
			_ = collectAll(t, iter)
		}
		done <- true
	}()

	// Wait for both
	<-done
	<-done
}

// TestClose_CalledTwice_Panics is a regression test for GitHub issue #48:
// Close unconditionally called close(m.bus), so calling it a second time
// panicked with "close of closed channel", contrary to the EventStore
// contract that Close must be idempotent.
func TestClose_CalledTwice_Panics(t *testing.T) {
	store := memory.NewMemoryStore(10)

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

// TestSave_AfterClose_Panics is a regression test for GitHub issue #48:
// Save's non-blocking send to m.bus only guarded against a full channel,
// not a closed one, so any Save call after Close panicked with "send on
// closed channel" instead of returning an error.
func TestSave_AfterClose_Panics(t *testing.T) {
	store := memory.NewMemoryStore(10)

	if err := store.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Save after Close panicked: %v", r)
		}
	}()

	_, err := store.Save(context.Background(), []cqrs.Envelope{
		{StreamID: "order-1", Event: nil},
	}, cqrs.Any{})
	if err == nil {
		t.Error("expected an error saving to a closed store, got nil")
	}
}

// TestLoadFromAll_Any is a regression test for GitHub issue #49:
// LoadFromAll never switched on the concrete type of version, feeding
// version.ToRawInt64() straight into an unsigned offset. Any{}'s
// ToRawInt64() of -1 became a huge uint64 offset, panicking with
// "index out of range" on the very first Next() call — even against an
// empty store.
func TestLoadFromAll_Any(t *testing.T) {
	ctx := context.Background()

	t.Run("empty store", func(t *testing.T) {
		store := memory.NewMemoryStore(10)

		iter, err := store.LoadFromAll(ctx, cqrs.Any{})
		if err != nil {
			t.Fatalf("LoadFromAll(Any{}) on an empty store: unexpected error: %v", err)
		}

		got := collectAll(t, iter)
		if len(got) != 0 {
			t.Fatalf("expected 0 events, got %d", len(got))
		}
	})

	t.Run("store with events", func(t *testing.T) {
		store := memory.NewMemoryStore(10)

		if _, err := store.Save(ctx, []cqrs.Envelope{
			newEnvelope("order-1", OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
			newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 2}),
		}, cqrs.NoStream{}); err != nil {
			t.Fatalf("setup save: %v", err)
		}

		iter, err := store.LoadFromAll(ctx, cqrs.Any{})
		if err != nil {
			t.Fatalf("LoadFromAll(Any{}): unexpected error: %v", err)
		}

		got := collectAll(t, iter)
		if len(got) != 2 {
			t.Fatalf("expected 2 events, got %d", len(got))
		}
	})
}
// TestLoadStreamFrom_RevisionAtStreamHead is a regression test for GitHub
// issue #50: LoadStreamFrom's Revision guard used >= where a start index
// needs >, so asking for the revision a stream is currently at — "I am
// caught up, give me anything newer" — returned ErrInvalidRevision instead
// of an empty iterator. Revision(0) against a fresh (never-saved) stream
// made it impossible to create a new aggregate through NewCommandHandler
// configured with WithStreamState(Revision(0)).
func TestLoadStreamFrom_RevisionAtStreamHead(t *testing.T) {
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
			store := memory.NewMemoryStore(10)
			defer store.Close()

			if tt.eventCount > 0 {
				events := make([]cqrs.Envelope, tt.eventCount)
				for i := range events {
					events[i] = newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
				}
				if _, err := store.Save(context.Background(), events, cqrs.Any{}); err != nil {
					t.Fatalf("setup save: %v", err)
				}
			}

			// The stream is at revision tt.eventCount. Saving against it is legal,
			// so loading from it must be too.
			iter, err := store.LoadStreamFrom(context.Background(), "order-1", cqrs.Revision(tt.eventCount))
			if err != nil {
				t.Fatalf("LoadStreamFrom(Revision(%d)) on a stream with %d events: "+
					"expected an empty iterator, got error: %v", tt.eventCount, tt.eventCount, err)
			}

			if loaded := collectAll(t, iter); len(loaded) != 0 {
				t.Errorf("expected 0 events past the head, got %d", len(loaded))
			}
		})
	}
}

// TestLoadFromAll_RevisionAtHead is the LoadFromAll counterpart of
// TestLoadStreamFrom_RevisionAtStreamHead: the same off-by-one guard backed
// both.
func TestLoadFromAll_RevisionAtHead(t *testing.T) {
	tests := []struct {
		name       string
		eventCount int
	}{
		{name: "empty store, position 0", eventCount: 0},
		{name: "two events, position 2", eventCount: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := memory.NewMemoryStore(10)
			defer store.Close()

			for i := 0; i < tt.eventCount; i++ {
				_, _ = store.Save(context.Background(), []cqrs.Envelope{
					newEnvelope("order-1", ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i}),
				}, cqrs.Any{})
			}

			iter, err := store.LoadFromAll(context.Background(), cqrs.Revision(tt.eventCount))
			if err != nil {
				t.Fatalf("LoadFromAll(Revision(%d)) with %d events stored: "+
					"expected an empty iterator, got error: %v", tt.eventCount, tt.eventCount, err)
			}

			if loaded := collectAll(t, iter); len(loaded) != 0 {
				t.Errorf("expected 0 events past the head, got %d", len(loaded))
			}
		})
	}
}

// --- End-to-end impact: an aggregate can never be created with Revision-based
// optimistic concurrency, because the very first load of the empty stream fails.

type createOrder struct{ ID string }

func (c createOrder) AggregateID() string { return c.ID }
func (c createOrder) CommandType() string { return "createOrder" }

func TestCommandHandler_CreateAggregateWithRevisionZero(t *testing.T) {
	store := memory.NewMemoryStore(10)
	defer store.Close()

	handler := cqrs.NewCommandHandler(
		cqrs.EventStore(store),
		func() int { return 0 },
		func(state int, _ *cqrs.Envelope) int { return state + 1 },
		func(state int, cmd createOrder) ([]cqrs.Event, error) {
			return []cqrs.Event{OrderCreated{OrderID: cmd.ID, CustomerID: "cust-1"}}, nil
		},
		cqrs.WithStreamState(cqrs.Revision(0)),
	)

	result, err := handler(context.Background(), createOrder{ID: "order-1"})
	if err != nil {
		t.Fatalf("creating a brand new aggregate at Revision(0) must succeed, got: %v", err)
	}
	if !result.Successful {
		t.Errorf("expected successful result, got %+v", result)
	}
	if errors.Is(err, cqrs.ErrInvalidRevision) {
		t.Errorf("load of the empty stream rejected the initial revision")
	}
}
