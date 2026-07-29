package file

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

type orderPlaced struct {
	ID    string `json:"id"`
	Total int    `json:"total"`
}

func (e orderPlaced) AggregateID() string { return e.ID }
func (e orderPlaced) EventType() string   { return "order_placed" }

func init() {
	eventsourcing.RegisterEventByType(func() eventsourcing.Event { return &orderPlaced{} })
}

// TestFileEventBusDeliversDispatchedEvent is a regression test for GitHub
// issue #37: Envelope.Event is a bare interface with no custom unmarshaller,
// so json.Unmarshal into a fresh Envelope always failed, and processFile
// swallowed the error and returned — no subscriber ever received any event,
// and every dispatched file accumulated on disk forever.
func TestFileEventBusDeliversDispatchedEvent(t *testing.T) {
	root := t.TempDir()
	bus, err := NewFileEventBus(root)
	if err != nil {
		t.Fatalf("NewFileEventBus: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close() })

	got := make(chan eventsourcing.Event, 1)
	err = bus.Subscribe(
		context.Background(),
		"projector",
		eventsourcing.NewEventHandlerFunc(func(ctx context.Context, ev eventsourcing.Event) error {
			select {
			case got <- ev:
			default:
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Let the subscriber's fsnotify watcher come up.
	time.Sleep(200 * time.Millisecond)

	env := &eventsourcing.Envelope{
		EventID:       uuid.New(),
		StreamID:      "order-1",
		Event:         orderPlaced{ID: "order-1", Total: 42},
		Version:       1,
		GlobalVersion: 1,
		OccurredAt:    time.Now().UTC(),
	}

	if err := bus.Dispatch(env); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	select {
	case ev := <-got:
		if ev == nil {
			t.Fatalf("handler received a nil event")
		}
		if ev.EventType() != "order_placed" {
			t.Fatalf("handler received %T (%q), want order_placed", ev, ev.EventType())
		}
	case <-time.After(3 * time.Second):
		// Distinguish "Dispatch never wrote the file" from "the worker wrote it
		// but could never decode it". The latter was the actual bug.
		left, _ := os.ReadDir(filepath.Join(root, "projector"))
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Fatalf("handler never received the dispatched event; undelivered files still on disk: %v", names)
	}
}
