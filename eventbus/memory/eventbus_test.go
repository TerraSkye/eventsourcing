package memory

import (
	"context"
	"sync"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
)

type blockingEvent struct{}

func (blockingEvent) AggregateID() string { return "agg-1" }
func (blockingEvent) EventType() string   { return "blockingEvent" }

type blockingHandler struct {
	handling     chan struct{}
	handlingOnce sync.Once
	release      chan struct{}
}

func (h *blockingHandler) Handle(ctx context.Context, ev cqrs.Event) error {
	h.handlingOnce.Do(func() { close(h.handling) })
	<-h.release
	return nil
}

// TestDispatch_BlockingSendDoesNotHoldLock is a regression test for GitHub
// issue #31: Dispatch performed a blocking send to a subscriber's channel
// while still holding b.mu.RLock(). A slow or stuck handler left that send
// (and the RLock) pending forever, deadlocking Close (which needs
// b.mu.Lock()) and every other call that needs the lock — not just the one
// Dispatch call for the slow subscriber.
func TestDispatch_BlockingSendDoesNotHoldLock(t *testing.T) {
	bus := NewEventBus(0) // unbuffered subscriber channel

	h := &blockingHandler{
		handling: make(chan struct{}),
		release:  make(chan struct{}),
	}

	if err := bus.Subscribe(context.Background(), "sub1", h); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First event: runSubscriber's select receives it immediately (synchronous
	// handoff on the unbuffered channel) and calls Handle, which blocks on
	// h.release. This first Dispatch call itself returns fine.
	bus.Dispatch(&cqrs.Envelope{Event: blockingEvent{}})

	select {
	case <-h.handling:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	// Second event: runSubscriber is busy inside Handle and not receiving
	// from s.events, and the channel is unbuffered, so this send blocks. It
	// must not do so while holding b.mu.
	go bus.Dispatch(&cqrs.Envelope{Event: blockingEvent{}})
	time.Sleep(200 * time.Millisecond)

	// A completely unrelated call that needs b.mu.Lock() must not be blocked
	// by the pending send above.
	subscribeDone := make(chan error, 1)
	go func() {
		subscribeDone <- bus.Subscribe(context.Background(), "sub2", cqrs.NewEventHandlerFunc(
			func(ctx context.Context, ev cqrs.Event) error { return nil },
		))
	}()

	select {
	case err := <-subscribeDone:
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe was blocked by the pending Dispatch send — b.mu is still held during the blocking send")
	}

	close(h.release)

	closeDone := make(chan struct{})
	go func() {
		_ = bus.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not complete after the handler was released")
	}
}
