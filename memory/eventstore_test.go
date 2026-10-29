package memory_test

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/eventsourcingtest"
	"github.com/terraskye/eventsourcing/memory"
)

// Test event types

type OrderCreated struct {
	OrderID    string
	CustomerID string
}

func (e *OrderCreated) AggregateID() string { return e.OrderID }
func (e *OrderCreated) EventType() string   { return "OrderCreated" }

type ItemAdded struct {
	OrderID string
	ItemID  string
	Qty     int
}

func (e *ItemAdded) AggregateID() string { return e.OrderID }
func (e *ItemAdded) EventType() string   { return "ItemAdded" }

type OrderShipped struct {
	OrderID string
}

func (e *OrderShipped) AggregateID() string { return e.OrderID }
func (e *OrderShipped) EventType() string   { return "OrderShipped" }

func init() {
	eventsourcing.RegisterEvent(&OrderCreated{})
	eventsourcing.RegisterEvent(&ItemAdded{})
	eventsourcing.RegisterEvent(&OrderShipped{})
	eventsourcing.RegisterEvent(&closeTestEvent{})
	eventsourcing.RegisterEvent(&aliasTestEvent{})
}

// Helper functions

func newEnvelope(streamID string, event eventsourcing.Event) eventsourcing.Envelope {
	return eventsourcing.Envelope{
		EventID:    uuid.New(),
		StreamID:   streamID,
		Event:      event,
		OccurredAt: time.Now(),
		Metadata:   map[string]any{},
	}
}

func collectAll(t *testing.T, iter *eventsourcing.Iterator[*eventsourcing.Envelope]) []*eventsourcing.Envelope {
	t.Helper()
	var results []*eventsourcing.Envelope
	for iter.Next() {
		results = append(results, iter.Value())
	}
	if err := iter.Err(); err != nil && err != io.EOF {
		t.Fatalf("iterator error: %v", err)
	}
	return results
}

// Save Tests

func TestSave_EmptySlice(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	result, err := store.Save(context.Background(), []eventsourcing.Envelope{}, eventsourcing.Any{})

	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_SingleEvent(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	event := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	result, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Any{})

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
	store := memory.NewEventStore()
	defer store.Close()

	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 2}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-2", Qty: 1}),
	}

	result, err := store.Save(context.Background(), events, eventsourcing.Any{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.NextExpectedVersion != 3 {
		t.Errorf("expected NextExpectedVersion 3, got %d", result.NextExpectedVersion)
	}
}

func TestSave_MixedStreamIDs_Fails(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-2", &OrderCreated{OrderID: "order-2", CustomerID: "cust-2"}),
	}

	result, err := store.Save(context.Background(), events, eventsourcing.Any{})

	if err == nil {
		t.Fatal("expected error for mixed stream IDs")
	}
	if !errors.Is(err, eventsourcing.ErrInvalidEventBatch) {
		t.Errorf("expected ErrInvalidEventBatch, got %v", err)
	}
	if result.Successful {
		t.Error("expected unsuccessful result")
	}
}

// Revision Tests

func TestSave_NoStream_Success(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	event := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	result, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.NoStream{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_NoStream_FailsWhenStreamExists(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Create stream first
	event1 := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{event1}, eventsourcing.Any{})

	// Try to save with NoStream
	event2 := newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1})
	_, err := store.Save(context.Background(), []eventsourcing.Envelope{event2}, eventsourcing.NoStream{})

	if err == nil {
		t.Fatal("expected error when stream already exists")
	}
	if !errors.Is(err, eventsourcing.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

func TestSave_StreamExists_Success(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Create stream first
	event1 := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{event1}, eventsourcing.Any{})

	// Append with StreamExists
	event2 := newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1})
	result, err := store.Save(context.Background(), []eventsourcing.Envelope{event2}, eventsourcing.StreamExists{})

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !result.Successful {
		t.Error("expected successful result")
	}
}

func TestSave_StreamExists_FailsWhenNoStream(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	event := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.StreamExists{})

	if err == nil {
		t.Fatal("expected error when stream doesn't exist")
	}
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestSave_Revision_Success(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Create stream with 2 events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Append at correct revision
	event := newEnvelope("order-1", &OrderShipped{OrderID: "order-1"})
	result, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Revision(2))

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if result.NextExpectedVersion != 3 {
		t.Errorf("expected NextExpectedVersion 3, got %d", result.NextExpectedVersion)
	}
}

func TestSave_Revision_Conflict(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Create stream with 2 events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Try to append at wrong revision
	event := newEnvelope("order-1", &OrderShipped{OrderID: "order-1"})
	_, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Revision(1))

	if err == nil {
		t.Fatal("expected conflict error")
	}

	var conflictErr *eventsourcing.StreamRevisionConflictError
	if !errors.As(err, &conflictErr) {
		t.Errorf("expected StreamRevisionConflictError, got %T: %v", err, err)
	}
}

// LoadStream Tests

func TestLoadStream_ExistingStream(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 2}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

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
	store := memory.NewEventStore()
	defer store.Close()

	_, err := store.LoadStream(context.Background(), "non-existing")

	if err == nil {
		t.Fatal("expected error for non-existing stream")
	}
	if !errors.Is(err, eventsourcing.ErrStreamNotFound) {
		t.Errorf("expected ErrStreamNotFound, got %v", err)
	}
}

func TestLoadStream_ContextCancellation(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save many events
	events := make([]eventsourcing.Envelope, 100)
	for i := range events {
		events[i] = newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Load with cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	iter, err := store.LoadStream(ctx, "order-1")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Read a few events then cancel
	iter.Next()
	iter.Next()
	cancel()

	// Next call should detect cancellation
	if iter.Next() {
		// Context check happens at start of Next, so it should return false
		if iter.Err() != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", iter.Err())
		}
	}
}

// LoadStreamFrom Tests

func TestLoadStreamFrom_AtVersion(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save 5 events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-2", Qty: 2}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-3", Qty: 3}),
		newEnvelope("order-1", &OrderShipped{OrderID: "order-1"}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Load from version 2 (0-indexed, so skip first 2)
	iter, err := store.LoadStreamFrom(context.Background(), "order-1", eventsourcing.Revision(2))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 3 {
		t.Errorf("expected 3 events, got %d", len(loaded))
	}

	// First event should be the third one (item-2)
	if itemAdded, ok := loaded[0].Event.(*ItemAdded); !ok || itemAdded.ItemID != "item-2" {
		t.Errorf("expected ItemAdded with item-2, got %+v", loaded[0].Event)
	}
}

func TestLoadStreamFrom_InvalidVersion(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save 2 events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Try to load from version beyond stream length
	_, err := store.LoadStreamFrom(context.Background(), "order-1", eventsourcing.Revision(10))

	if err == nil {
		t.Fatal("expected error for invalid version")
	}
	if !errors.Is(err, eventsourcing.ErrInvalidRevision) {
		t.Errorf("expected ErrInvalidRevision, got %v", err)
	}
}

// TestLoadStreamFrom_HugeRevisionPanicsInsteadOfErroring verifies that a
// Revision whose uint64 value is >= 2^63 (which becomes negative once
// ToRawInt64 converts it to int64) is rejected eagerly with
// eventsourcing.ErrInvalidRevision, the same way LoadStreamFrom already rejects
// smaller out-of-range revisions, rather than deferring an error to the
// iterator or panicking.
func TestLoadStreamFrom_HugeRevisionPanicsInsteadOfErroring(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// A Revision whose uint64 value is >= 2^63 becomes negative once
	// ToRawInt64 converts it to int64 -- here, exactly -1, the same sentinel
	// LoadStreamFrom's bounds check (and, separately, Any{}.ToRawInt64())
	// uses. The out-of-range check is expected to catch this and return
	// eventsourcing.ErrInvalidRevision eagerly, the same way it already does for
	// small out-of-range revisions above -- the store never defers this
	// error to the iterator.
	rev := eventsourcing.Revision(math.MaxUint64)

	iter, err := store.LoadStreamFrom(context.Background(), "nonexistent-stream", rev)
	if iter != nil {
		t.Fatalf("expected a nil iterator when LoadStreamFrom rejects the revision")
	}
	if !errors.Is(err, eventsourcing.ErrInvalidRevision) {
		t.Fatalf("expected ErrInvalidRevision, got %v", err)
	}
}

func TestLoadStreamFrom_StreamExists(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Load with StreamExists
	iter, err := store.LoadStreamFrom(context.Background(), "order-1", eventsourcing.StreamExists{})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	loaded := collectAll(t, iter)
	if len(loaded) != 1 {
		t.Errorf("expected 1 event, got %d", len(loaded))
	}
}

func TestLoadStreamFrom_NoStream(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save events
	events := []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}
	_, _ = store.Save(context.Background(), events, eventsourcing.Any{})

	// Load with NoStream should fail
	_, err := store.LoadStreamFrom(context.Background(), "order-1", eventsourcing.NoStream{})

	if err == nil {
		t.Fatal("expected error for NoStream on existing stream")
	}
	if !errors.Is(err, eventsourcing.ErrStreamExists) {
		t.Errorf("expected ErrStreamExists, got %v", err)
	}
}

// ReadAll Tests

func TestReadAll_AllEvents(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save events to multiple streams
	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
	}, eventsourcing.Any{})

	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{
		newEnvelope("order-2", &OrderCreated{OrderID: "order-2", CustomerID: "cust-2"}),
	}, eventsourcing.Any{})

	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}, eventsourcing.Any{})

	// Read all from the beginning
	loaded := readAll(t, store, 0)
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

func TestReadAll_AfterPosition(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// Save 5 events
	for i := 0; i < 5; i++ {
		_, _ = store.Save(context.Background(), []eventsourcing.Envelope{
			newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i}),
		}, eventsourcing.Any{})
	}

	// Read after position 2
	loaded := readAll(t, store, 2)
	if len(loaded) != 3 {
		t.Errorf("expected 3 events, got %d", len(loaded))
	}
}

// TestReadAll_AfterHead reads after a position beyond the head: an empty
// page at the end of the log, with Next left where it was, rather than an
// error, since a position past the head is simply nothing new yet.
func TestReadAll_AfterHead(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item-1", Qty: 1}),
	}, eventsourcing.Any{})

	for _, after := range []uint64{2, 10} {
		res, err := store.ReadAll(context.Background(), after, eventsourcing.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadAll(after %d): %v", after, err)
		}
		if len(res.Events) != 0 || !res.AtEnd || res.Next != after {
			t.Errorf("ReadAll(after %d) = %d events, Next %d, AtEnd %v; want 0, %d, true",
				after, len(res.Events), res.Next, res.AtEnd, after)
		}
	}
}

// Close Tests

func TestClose(t *testing.T) {
	store := memory.NewEventStore()

	err := store.Close()
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}

	event := newEnvelope("order-1", &OrderCreated{OrderID: "order-1"})
	if _, err := store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Any{}); err == nil {
		t.Error("Save after Close succeeded, want an error")
	}
}

// Concurrency Tests

func TestConcurrent_Saves(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	done := make(chan bool)
	numGoroutines := 10
	eventsPerGoroutine := 10

	for i := 0; i < numGoroutines; i++ {
		go func(streamNum int) {
			streamID := "order-" + string(rune('A'+streamNum))
			for j := 0; j < eventsPerGoroutine; j++ {
				event := newEnvelope(streamID, &ItemAdded{OrderID: streamID, ItemID: "item", Qty: j})
				_, _ = store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Any{})
			}
			done <- true
		}(i)
	}

	// Wait for all goroutines
	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	// Verify total events
	loaded := readAll(t, store, 0)
	expected := numGoroutines * eventsPerGoroutine
	if len(loaded) != expected {
		t.Errorf("expected %d events, got %d", expected, len(loaded))
	}
}

func TestConcurrent_SaveAndLoad(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	// First create the stream
	event := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	_, _ = store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Any{})

	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 50; i++ {
			event := newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
			_, _ = store.Save(context.Background(), []eventsourcing.Envelope{event}, eventsourcing.Any{})
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
	store := memory.NewEventStore()

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
	store := memory.NewEventStore()

	if err := store.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Save after Close panicked: %v", r)
		}
	}()

	_, err := store.Save(context.Background(), []eventsourcing.Envelope{
		{StreamID: "order-1", Event: nil},
	}, eventsourcing.Any{})
	if err == nil {
		t.Error("expected an error saving to a closed store, got nil")
	}
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
			store := memory.NewEventStore()
			defer store.Close()

			if tt.eventCount > 0 {
				events := make([]eventsourcing.Envelope, tt.eventCount)
				for i := range events {
					events[i] = newEnvelope("order-1", &ItemAdded{OrderID: "order-1", ItemID: "item", Qty: i})
				}
				if _, err := store.Save(context.Background(), events, eventsourcing.Any{}); err != nil {
					t.Fatalf("setup save: %v", err)
				}
			}

			// The stream is at revision tt.eventCount. Saving against it is legal,
			// so loading from it must be too.
			iter, err := store.LoadStreamFrom(context.Background(), "order-1", eventsourcing.Revision(tt.eventCount))
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

// --- End-to-end impact: an aggregate can never be created with Revision-based
// optimistic concurrency, because the very first load of the empty stream fails.

type createOrder struct{ ID string }

func (c createOrder) AggregateID() string { return c.ID }
func (c createOrder) CommandType() string { return "createOrder" }

func TestCommandHandler_CreateAggregateWithRevisionZero(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	handler := eventsourcing.NewCommandHandler(
		eventsourcing.EventStore(store),
		func() int { return 0 },
		func(state int, _ *eventsourcing.Envelope) int { return state + 1 },
		func(state int, cmd createOrder) ([]eventsourcing.Event, error) {
			return []eventsourcing.Event{&OrderCreated{OrderID: cmd.ID, CustomerID: "cust-1"}}, nil
		},
		eventsourcing.WithStreamState(eventsourcing.Revision(0)),
	)

	result, err := handler(context.Background(), createOrder{ID: "order-1"})
	if err != nil {
		t.Fatalf("creating a brand new aggregate at Revision(0) must succeed, got: %v", err)
	}
	if !result.Successful {
		t.Errorf("expected successful result, got %+v", result)
	}
	if errors.Is(err, eventsourcing.ErrInvalidRevision) {
		t.Errorf("load of the empty stream rejected the initial revision")
	}
}

type closeTestEvent struct {
	ID string
}

func (e *closeTestEvent) AggregateID() string { return e.ID }
func (e *closeTestEvent) EventType() string   { return "closeTestEvent" }

// TestClose_LeavesGlobalEventsAccessible verifies Close's doc comment
// promise that "Close discards all stored events" holds for the global
// event log ReadAll reads, not just per-stream reads.
func TestClose_LeavesGlobalEventsAccessible(t *testing.T) {

	store := memory.NewEventStore()
	ctx := context.Background()

	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		{EventID: uuid.New(), StreamID: "s1", Event: &closeTestEvent{ID: "s1"}, Version: 1},
	}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Close's doc comment: "Close discards all stored events", the global
	// log included.
	if events := readAll(t, store, 0); len(events) != 0 {
		t.Fatalf("Close should discard all stored events, but ReadAll after Close returned %d event(s)", len(events))
	}
}

type aliasTestEvent struct {
	ID    string
	Value string
}

func (e *aliasTestEvent) AggregateID() string { return e.ID }
func (e *aliasTestEvent) EventType() string   { return "aliasTestEvent" }

// TestSave_ReusedCallerSliceCorruptsAllPreviouslySavedEnvelopes verifies
// that Save copies each Envelope value rather than storing a pointer into
// the caller's events slice, so a caller that reuses the same backing
// slice across multiple Save calls (a common buffer-reuse pattern) doesn't
// see every previously "saved" envelope silently change to reflect the
// latest Save call's contents.
func TestSave_ReusedCallerSliceCorruptsAllPreviouslySavedEnvelopes(t *testing.T) {

	store := memory.NewEventStore()
	ctx := context.Background()

	// A single-element buffer reused across independent Save calls, as a
	// caller might do to avoid allocating a new slice per aggregate.
	buf := make([]eventsourcing.Envelope, 1)

	for i, id := range []string{"agg-1", "agg-2", "agg-3"} {
		buf[0] = eventsourcing.Envelope{
			EventID:  uuid.New(),
			StreamID: id,
			Event:    &aliasTestEvent{ID: id, Value: id},
			Version:  uint64(i + 1),
		}
		if _, err := store.Save(ctx, buf, eventsourcing.NoStream{}); err != nil {
			t.Fatalf("save %q failed: %v", id, err)
		}
	}

	// Each stream should independently retain the value it was saved with.
	for _, id := range []string{"agg-1", "agg-2", "agg-3"} {
		iter, err := store.LoadStream(ctx, id)
		if err != nil {
			t.Fatalf("load stream %q failed: %v", id, err)
		}
		envs := collectAll(t, iter)
		if len(envs) != 1 {
			t.Fatalf("stream %q: expected 1 event, got %d", id, len(envs))
		}
		got := envs[0].Event.(*aliasTestEvent).Value
		if got != id {
			t.Errorf("stream %q: expected stored event value %q, got %q (corrupted by a later Save reusing the same slice)", id, id, got)
		}
	}
}

func TestAcceptance(t *testing.T) {
	store := memory.NewEventStore()
	t.Cleanup(func() { store.Close() })

	eventsourcingtest.AcceptanceTest(t, store)
}

func TestGlobalReaderAcceptance(t *testing.T) {
	store := memory.NewEventStore()
	t.Cleanup(func() { store.Close() })

	eventsourcingtest.GlobalReaderAcceptanceTest(t, store, store)
}

func TestCorrelationLoaderAcceptance(t *testing.T) {
	store := memory.NewEventStore()
	t.Cleanup(func() { store.Close() })

	eventsourcingtest.CorrelationLoaderAcceptanceTest(t, store, store)
}

// Save copied each envelope into the store before assigning its
// GlobalPosition, and assigned it to the caller's slice instead, so every
// stored event read back with GlobalPosition 0.
func TestSave_StoresGlobalPosition(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()
	ctx := context.Background()

	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-1", &OrderCreated{OrderID: "order-1"}),
		newEnvelope("order-1", &ItemAdded{OrderID: "order-1"}),
	}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("save order-1: %v", err)
	}
	if _, err := store.Save(ctx, []eventsourcing.Envelope{
		newEnvelope("order-2", &OrderCreated{OrderID: "order-2"}),
	}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("save order-2: %v", err)
	}

	all := readAll(t, store, 0)
	if len(all) != 3 {
		t.Fatalf("ReadAll returned %d events, want 3", len(all))
	}
	for i, env := range all {
		if want := uint64(i + 1); env.GlobalPosition != want {
			t.Errorf("ReadAll event %d: GlobalPosition = %d, want %d", i, env.GlobalPosition, want)
		}
	}

	iter, err := store.LoadStream(ctx, "order-2")
	if err != nil {
		t.Fatalf("LoadStream: %v", err)
	}
	stream := collectAll(t, iter)
	if len(stream) != 1 || stream[0].GlobalPosition != 3 {
		t.Errorf("LoadStream(order-2) = %v, want one event with GlobalPosition 3", stream)
	}
}

// LoadStreamFrom returned the stored *Envelope itself, so a caller that
// changed the envelope, its event or its metadata changed what every later
// read of the stream returned.
func TestLoadStream_ReturnsCopiesOfStoredEvents(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()
	ctx := context.Background()

	env := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	env.Metadata = map[string]any{"tenant": "t1"}
	if _, err := store.Save(ctx, []eventsourcing.Envelope{env}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	load := func() *eventsourcing.Envelope {
		t.Helper()
		iter, err := store.LoadStream(ctx, "order-1")
		if err != nil {
			t.Fatalf("LoadStream: %v", err)
		}
		envs := collectAll(t, iter)
		if len(envs) != 1 {
			t.Fatalf("LoadStream returned %d events, want 1", len(envs))
		}
		return envs[0]
	}

	first := load()
	first.Event.(*OrderCreated).CustomerID = "changed"
	first.Metadata["tenant"] = "changed"
	first.StreamID = "changed"

	second := load()
	if got := second.Event.(*OrderCreated).CustomerID; got != "cust-1" {
		t.Errorf("second read CustomerID = %q, want %q", got, "cust-1")
	}
	if got := second.Metadata["tenant"]; got != "t1" {
		t.Errorf("second read Metadata[tenant] = %v, want %q", got, "t1")
	}
	if second.StreamID != "order-1" {
		t.Errorf("second read StreamID = %q, want %q", second.StreamID, "order-1")
	}
}

// ReadAll copied each envelope but not the event or metadata it pointed to,
// so a caller that changed either changed what every later read returned.
func TestReadAll_ReturnsCopiesOfStoredEvents(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()
	ctx := context.Background()

	env := newEnvelope("order-1", &OrderCreated{OrderID: "order-1", CustomerID: "cust-1"})
	env.Metadata = map[string]any{"tenant": "t1"}
	if _, err := store.Save(ctx, []eventsourcing.Envelope{env}, eventsourcing.NoStream{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	first := readAll(t, store, 0)
	first[0].Event.(*OrderCreated).CustomerID = "changed"
	first[0].Metadata["tenant"] = "changed"

	second := readAll(t, store, 0)
	if got := second[0].Event.(*OrderCreated).CustomerID; got != "cust-1" {
		t.Errorf("second read CustomerID = %q, want %q", got, "cust-1")
	}
	if got := second[0].Metadata["tenant"]; got != "t1" {
		t.Errorf("second read Metadata[tenant] = %v, want %q", got, "t1")
	}
}

// Save rejects an event whose type was never registered, as the persistent
// stores do, rather than storing an event it could not decode on read.
func TestSave_RejectsUnregisteredEvent(t *testing.T) {
	store := memory.NewEventStore()
	defer store.Close()

	env := newEnvelope("order-1", &unregisteredEvent{ID: "order-1"})
	if _, err := store.Save(context.Background(), []eventsourcing.Envelope{env}, eventsourcing.NoStream{}); !errors.Is(err, eventsourcing.ErrEventNotRegistered) {
		t.Fatalf("Save error = %v, want ErrEventNotRegistered", err)
	}
	if events := readAll(t, store, 0); len(events) != 0 {
		t.Errorf("ReadAll after rejected Save returned %d events, want 0", len(events))
	}
}

type unregisteredEvent struct{ ID string }

func (e *unregisteredEvent) AggregateID() string { return e.ID }
func (e *unregisteredEvent) EventType() string   { return "unregisteredEvent" }

// readAll pages through the store's global log after position after and
// returns every event.
func readAll(t *testing.T, store *memory.EventStore, after uint64) []*eventsourcing.Envelope {
	t.Helper()
	var out []*eventsourcing.Envelope
	for {
		res, err := store.ReadAll(context.Background(), after, eventsourcing.ReadOptions{})
		if err != nil {
			t.Fatalf("ReadAll(after %d): %v", after, err)
		}
		out = append(out, res.Events...)
		after = res.Next
		if res.AtEnd {
			return out
		}
	}
}
