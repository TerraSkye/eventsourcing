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

func (c CartCreated) EventType() string { return "CartCreated" }

func (c CartCreated) AggregateID() string { return c.ID }

type ItemAdded struct {
	ID string
}

func (i *ItemAdded) AggregateID() string { return i.ID }
func (i *ItemAdded) EventType() string   { return "ItemAdded" }

type UnhandledEvent struct{}

func (o *UnhandledEvent) AggregateID() string { return "" }
func (o *UnhandledEvent) EventType() string   { return "UnhandledEvent" }

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

	var skipped *SkippedEventError

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

	var expected *SkippedEventError

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

// TestEventGroupProcessor_StreamFilter_Sorted asserts StreamFilter returns
// every registered name (see [EventNamesFor]) for each handled type, sorted
// — including every alias a single concrete event struct is registered
// under, such as the ItemAddedV2 alias added below for ItemAdded.
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

// TestEventGroupProcessor_StreamFilter_UnregisteredIsOmitted documents
// current behavior: a handled type never passed to
// RegisterEvent/RegisterEventByType/RegisterEventByName isn't found in the
// global registry, and StreamFilter has no fallback to that type's own
// EventType() — it's silently omitted from the result instead.
func TestEventGroupProcessor_StreamFilter_UnregisteredIsOmitted(t *testing.T) {
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	RegisterEvent(&CartCreated{})
	// ItemAdded intentionally left unregistered.

	group := NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev *ItemAdded) error { return nil }),
		OnEvent(func(ctx context.Context, ev *CartCreated) error { return nil }),
	)

	names := group.StreamFilter()
	expected := []string{"CartCreated"}
	if !reflect.DeepEqual(names, expected) {
		t.Errorf("StreamFilter() = %v, want %v (ItemAdded is unregistered and silently omitted)", names, expected)
	}
}

// TestNewEventHandlerFunc_NotUsableInGroupProcessor is a regression test for
// GitHub issue #36: NewEventHandlerFunc's own godoc example showed its
// result being passed to NewEventGroupProcessor, which panics because the
// returned handler has no single EventName to route by (unlike a handler
// built with OnEvent). The fix corrected the misleading example rather than
// the panic — a handler that processes every event type, unfiltered,
// genuinely cannot report the one event name EventGroupProcessor routes by.
// This test documents that this panic is expected, and that
// NewEventHandlerFunc's handler works fine on its own (e.g. via a direct
// Handle call, as the corrected godoc example shows via EventBus.Subscribe).
func TestNewEventHandlerFunc_NotUsableInGroupProcessor(t *testing.T) {
	handler := NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		return nil
	})

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected NewEventGroupProcessor to panic on a NewEventHandlerFunc handler")
		}
	}()
	NewEventGroupProcessor(handler)
}

func TestNewEventHandlerFunc_UsableDirectly(t *testing.T) {
	var handled Event
	handler := NewEventHandlerFunc(func(ctx context.Context, ev Event) error {
		handled = ev
		return nil
	})

	if err := handler.Handle(context.Background(), CartCreated{ID: "123"}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if handled != (CartCreated{ID: "123"}) {
		t.Errorf("handled = %v, want %v", handled, CartCreated{ID: "123"})
	}
}

// TestEventGroupProcessor_StreamFilter_ValueHandlerMissesPointerRegisteredAliases
// documents a bug: see
// .bug/streamfilter-value-handler-misses-pointer-registered-aliases.md.
//
// StreamFilter's doc comment promises that "for a handled type registered in
// the global event registry ... it includes every name that type is
// registered under, since one concrete event struct can be registered under
// several names (for example after a rename, via RegisterEventByName)".
//
// CartCreated's EventType/AggregateID are value-receiver (see
// event_handler_test.go), so it can be used with OnEvent either as a pointer
// type parameter (OnEvent(func(ctx, ev *CartCreated) ...), the shape every
// other StreamFilter test in this package uses) or, equally legally, as a
// value type parameter (OnEvent(func(ctx, ev CartCreated) ...)). But
// RegisterEvent(&CartCreated{}) — the idiomatic registration call, and the
// only kind used anywhere in this codebase's own examples — always registers
// the type in typeToNames under its *pointer* form's %T string
// ("*eventsourcing.CartCreated"), because RegisterEventByType's factory
// closure always returns a pointer.
//
// EventNamesFor keys strictly on %T of the instance passed to it. A
// value-type OnEvent handler's EventInstance() returns a zero value T (a bare
// CartCreated{}, not a pointer), whose %T is "eventsourcing.CartCreated" —
// missing the leading asterisk, so it never matches the pointer-keyed
// registry entry. StreamFilter() then silently falls through to the
// unregistered-type fallback (instance.EventType()), which only ever reports
// the single primary name, dropping every additional alias added via
// RegisterEventByName — even though CartCreated genuinely is registered, and
// even though the exact same handler built with a pointer type parameter
// would report every alias correctly (see
// TestEventGroupProcessor_StreamFilter_Sorted, which asserts exactly that for
// the pointer form).
func TestEventGroupProcessor_StreamFilter_ValueHandlerMissesPointerRegisteredAliases(t *testing.T) {

	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	RegisterEvent(&CartCreated{})
	RegisterEventByName("LegacyCartCreated", func() Event {
		return &CartCreated{}
	})

	group := NewEventGroupProcessor(
		// Value type parameter — legal since CartCreated's methods are
		// value-receiver, but a different Go type than the *CartCreated form
		// RegisterEvent registered under.
		OnEvent(func(ctx context.Context, ev CartCreated) error { return nil }),
	)

	// Handle still routes correctly: this isn't about the handler being
	// broken, only about StreamFilter() misreporting its aliases.
	if err := group.Handle(context.Background(), CartCreated{ID: "c1"}); err != nil {
		t.Fatalf("Handle: unexpected error: %v", err)
	}

	names := group.StreamFilter()
	expected := []string{"CartCreated", "LegacyCartCreated"}
	if !reflect.DeepEqual(names, expected) {
		t.Fatalf("StreamFilter() = %v, want %v: the LegacyCartCreated alias registered via "+
			"RegisterEventByName is silently missing because the value-type handler's "+
			"EventInstance() doesn't match the pointer-keyed registry entry", names, expected)
	}
}

// TestStreamFilter_UnregisteredPointerHandlerOfValueReceiverEventIsOmitted
// covers the combination that used to panic: a handler registered through
// OnEvent with a pointer type parameter, over an Event whose methods take a
// value receiver, whose concrete type was never registered.
//
// StreamFilter once fell back to calling EventType() on the instance when
// the registry had no name for it. EventInstance() returns the zero T, which
// for T = *CartCreated is a nil pointer, and reaching a value-receiver
// method through it dereferences the nil before the method body runs --
// "value method CartCreated.EventType called using nil *CartCreated
// pointer".
//
// There is no fallback now: a filter name has to come from the registry, so
// an unregistered type contributes nothing and the nil instance is never
// called. This test pins both halves -- the omission, and that getting there
// does not panic.
func TestStreamFilter_UnregisteredPointerHandlerOfValueReceiverEventIsOmitted(t *testing.T) {

	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	// CartCreated's AggregateID/EventType methods take a value receiver, and
	// it is intentionally left unregistered.
	group := NewEventGroupProcessor(
		OnEvent(func(ctx context.Context, ev *CartCreated) error { return nil }),
	)

	if names := group.StreamFilter(); len(names) != 0 {
		t.Fatalf("StreamFilter() = %v, want no names: the type is unregistered, "+
			"so it has no name a store would have persisted events under", names)
	}
}

// nameOnlyEventHandler is an EventHandler that implements the EventName()
// method NewEventGroupProcessor requires for routing, but deliberately does
// NOT implement EventInstance() — unlike every handler built by [OnEvent].
// This is a realistic shape for a hand-rolled EventHandler, or a wrapper
// around an OnEvent handler that forwards EventName() (needed to pass
// NewEventGroupProcessor's construction-time check) but doesn't also forward
// EventInstance().
type nameOnlyEventHandler struct {
	name    string
	called  bool
	handled Event
}

func (h *nameOnlyEventHandler) Handle(ctx context.Context, ev Event) error {
	h.called = true
	h.handled = ev
	return nil
}

func (h *nameOnlyEventHandler) EventName() string {
	return h.name
}

// TestStreamFilter_HandlerWithoutEventInstance covers a handler that
// implements only the EventName method NewEventGroupProcessor requires, and
// never the EventInstance one every OnEvent handler happens to have.
// StreamFilter used to consider only handlers that type-asserted to
// EventInstance, so such a handler was skipped outright — its type could not
// reach the filter even when registered.
//
// It is resolved through its routing key now, which is the same type key the
// registry files names under. Registration still decides: a registered type
// contributes every name it is registered under, an unregistered one
// contributes nothing, exactly as for an OnEvent handler.
func TestStreamFilter_HandlerWithoutEventInstance(t *testing.T) {
	cartCreatedName := reflect.TypeOf(CartCreated{}).String()

	t.Run("registered type is included", func(t *testing.T) {
		registryMu.Lock()
		registry = map[string]func() Event{}
		typeToNames = map[string][]string{}
		registryMu.Unlock()

		RegisterEvent(&CartCreated{})
		RegisterEventByName("LegacyCartCreated", func() Event { return &CartCreated{} })

		h := &nameOnlyEventHandler{name: cartCreatedName}
		group := NewEventGroupProcessor(h)

		// Handle routes to h, so it is a full member of the group rather
		// than a degenerate non-participant.
		if err := group.Handle(context.Background(), CartCreated{ID: "c1"}); err != nil {
			t.Fatalf("Handle: unexpected error: %v", err)
		}
		if !h.called {
			t.Fatal("expected h.Handle to have been called for CartCreated")
		}

		names := group.StreamFilter()
		want := []string{"CartCreated", "LegacyCartCreated"}
		if !reflect.DeepEqual(names, want) {
			t.Fatalf("StreamFilter() = %v, want %v: a handler without EventInstance is "+
				"dropped from the filter even though its type is registered", names, want)
		}
	})

	t.Run("unregistered type is omitted", func(t *testing.T) {
		registryMu.Lock()
		registry = map[string]func() Event{}
		typeToNames = map[string][]string{}
		registryMu.Unlock()

		group := NewEventGroupProcessor(&nameOnlyEventHandler{name: cartCreatedName})

		if names := group.StreamFilter(); len(names) != 0 {
			t.Fatalf("StreamFilter() = %v, want no names: the type is unregistered, "+
				"so it has no name a store would have persisted events under", names)
		}
	})
}
