package eventsourcing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

var _ Event = (*CartCreated)(nil)
var _ Event = (*ItemAdded)(nil)
var _ Event = (*UnhandledEvent)(nil)

type CartCreated struct {
	ID string
}

func (c CartCreated) EventType() string { return TypeName(c) }

func (c CartCreated) AggregateID() string { return c.ID }

type ItemAdded struct {
	ID string
}

func (i *ItemAdded) AggregateID() string { return i.ID }
func (i *ItemAdded) EventType() string   { return TypeName(i) }

type UnhandledEvent struct{}

func (o *UnhandledEvent) AggregateID() string { return "" }
func (o *UnhandledEvent) EventType() string   { return TypeName(o) }

// --- Tests ---

type Projector struct{}

func (p Projector) OnItemAdded(ctx context.Context, ev *UnhandledEvent) error { return nil }
func (p Projector) OnCartCreated(ctx context.Context, ev *CartCreated) error  { return nil }
func (p Projector) OnEvent(ctx context.Context, ev Event) error               { return nil }

func TestEventNameExtraction(t *testing.T) {

	p := Projector{}

	h := OnEvent(p.OnCartCreated)

	u, ok := h.(interface{ EventName() string })
	if !ok {
		panic(fmt.Errorf("handler %T does not have a function `EventName()`", h))
	}

	if u.EventName() != "*eventsourcing.CartCreated" {
		t.Errorf("event name `CartCreated` does not match `EventName()`")
	}

}

func TestProjectorExample(t *testing.T) {

	p := Projector{}

	handler1 := NewEventGroupProcessor(
		OnEvent(p.OnCartCreated),
		OnEvent(p.OnItemAdded),
	)

	handler2 := NewEventHandlerFunc(p.OnEvent)

	handler1.Handle(context.Background(), &CartCreated{ID: "abc"})
	handler2.Handle(context.Background(), &CartCreated{ID: "abc"})
}

func TestTypedEventHandler_Handle_CorrectType(t *testing.T) {
	var called bool
	handler := OnEvent(func(ctx context.Context, ev *CartCreated) error {
		called = true
		return nil
	})

	err := handler.Handle(context.Background(), &CartCreated{ID: "abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("Handler should have been called")
	}
}

func TestTypedEventHandler_Handle_WrongType(t *testing.T) {
	handler := OnEvent(func(ctx context.Context, ev CartCreated) error {
		t.Fail() // should not be called
		return nil
	})

	var skipped *ErrSkippedEvent

	err := handler.Handle(context.Background(), &ItemAdded{ID: "xyz"})

	if !errors.As(err, &skipped) {
		t.Fatalf("expected skipped event, got %v", err)
	}

}

func TestEventGroupProcessor_RoutesEvents(t *testing.T) {
	calledCart := false
	calledItem := false

	group := NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev *CartCreated) error {
			calledCart = true
			return nil
		}),
		OnEvent(func(ctx context.Context, ev *ItemAdded) error {
			calledItem = true
			return nil
		}),
	)

	// Trigger CartCreated
	err := group.Handle(context.Background(), &CartCreated{ID: "c1"})
	if err != nil {
		t.Fatalf("CartCreated: unexpected error: %v", err)
	}
	if !calledCart {
		t.Error("expected calledCart to be true")
	}
	if calledItem {
		t.Error("expected calledItem to be false")
	}

	// Trigger ItemAdded
	err = group.Handle(context.Background(), &ItemAdded{ID: "i1"})
	if err != nil {
		t.Fatalf("ItemAdded: unexpected error: %v", err)
	}
	if !calledItem {
		t.Error("expected calledItem to be true")
	}
}

func TestEventGroupProcessor_SkippedEvent(t *testing.T) {
	group := NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
	)

	err := group.Handle(context.Background(), &UnhandledEvent{})

	var expected *ErrSkippedEvent

	if !errors.As(err, &expected) {
		t.Fatalf("expected skipped event, got %v", err)
	}
}

func TestEventGroupProcessor_DuplicateHandlerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate handler")
		}
	}()

	NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
		OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
	)
}

func TestEventGroupProcessor_StreamFilter_Sorted(t *testing.T) {
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	RegisterEvent(&CartCreated{})
	RegisterEvent(&ItemAdded{})

	group := NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev *ItemAdded) error { return nil }),
		OnEvent(func(ctx context.Context, ev *CartCreated) error { return nil }),
	)

	names := group.StreamFilter()
	expected := []string{"CartCreated", "ItemAdded"}
	if !reflect.DeepEqual(names, expected) {
		t.Errorf("StreamFilter() = %v, want %v", names, expected)
	}

	RegisterEventByName("ItemAddedV2", func() Event {
		return &ItemAdded{}
	})

	names = group.StreamFilter()
	expected = []string{"CartCreated", "ItemAdded", "ItemAddedV2"}
	if !reflect.DeepEqual(names, expected) {
		t.Errorf("StreamFilter() after RegisterEventByName = %v, want %v", names, expected)
	}
}
