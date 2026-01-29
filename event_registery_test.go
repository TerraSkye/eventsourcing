package eventsourcing

import (
	"strconv"
	"sync"
	"testing"
)

type TestEvent struct {
	ID string
}

func (e *TestEvent) EventType() string   { return "TestEvent" }
func (e *TestEvent) AggregateID() string { return e.ID }

// Another event for concurrency tests
type OtherEvent struct {
	Name string
}

func (e *OtherEvent) EventType() string   { return "OtherEvent" }
func (e *OtherEvent) AggregateID() string { return e.Name }

// --- Tests ---

func TestRegisterEventByType(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	t.Run("register and create new instance", func(t *testing.T) {
		RegisterEventByType(func() Event { return &TestEvent{} })

		ev, err := NewEventByName("TestEvent")
		if err != nil {
			t.Fatal(err)
		}

		if ev == nil {
			t.Fatal("expected non-nil event")
		}

		if _, ok := ev.(*TestEvent); !ok {
			t.Fatalf("expected *TestEvent, got %T", ev)
		}

		// Each call returns a new instance
		ev2, _ := NewEventByName("TestEvent")
		if ev == ev2 {
			t.Fatal("factory returned same instance twice")
		}
	})

	t.Run("panic on duplicate registration", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on duplicate registration")
			}
		}()
		RegisterEventByType(func() Event { return &TestEvent{} })
	})
}

func TestRegisterEventByName(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	t.Run("register by custom name", func(t *testing.T) {
		RegisterEventByName("Custom", func() Event { return &TestEvent{} })

		ev, err := NewEventByName("Custom")
		if err != nil {
			t.Fatal(err)
		}

		if ev == nil {
			t.Fatal("expected non-nil event")
		}

		if _, ok := ev.(*TestEvent); !ok {
			t.Fatalf("expected *TestEvent, got %T", ev)
		}
	})

	t.Run("panic on nil factory", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic on nil factory")
			}
		}()
		RegisterEventByName("NilFactory", nil)
	})
}

func TestNewEventByNameErrors(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registry["NilFactory"] = func() Event { return nil }
	registryMu.Unlock()

	_, err := NewEventByName("NonExistent")
	if err == nil {
		t.Fatal("expected error for unregistered event")
	}

	_, err2 := NewEventByName("NilFactory")
	if err2 == nil {
		t.Fatal("expected error for unregistered event")
	}

}

func TestConcurrencySafety(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "Evt" + strconv.Itoa(i)
			RegisterEventByName(name, func() Event { return &OtherEvent{Name: name} })
		}(i)
	}

	wg.Wait()

	// Verify all events are registered
	for i := 0; i < 100; i++ {
		name := "Evt" + strconv.Itoa(i)
		ev, err := NewEventByName(name)
		if err != nil {
			t.Fatalf("event %s not registered: %v", name, err)
		}
		if ev.(*OtherEvent).Name != name {
			t.Fatalf("event %s mismatch", name)
		}
	}
}

func TestEventNamesFor(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	RegisterEventByName("Name1", func() Event { return &TestEvent{} })
	RegisterEventByName("Name2", func() Event { return &TestEvent{} })
	RegisterEventByName("Other", func() Event { return &OtherEvent{} })

	names := EventNamesFor(&TestEvent{})
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}

	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["Name1"] || !found["Name2"] {
		t.Fatalf("expected Name1 and Name2, got %v", names)
	}

	otherNames := EventNamesFor(&OtherEvent{})
	if len(otherNames) != 1 || otherNames[0] != "Other" {
		t.Fatalf("expected [Other], got %v", otherNames)
	}

	// Unregistered type returns nil
	type UnknownEvent struct{}
	unknownNames := EventNamesFor(&TestEvent{ID: "unused"})
	// Same type, different value — should still match
	if len(unknownNames) != 2 {
		t.Fatalf("expected 2 names for same type with different value, got %d", len(unknownNames))
	}
}

func TestFactoryReturnsNil(t *testing.T) {
	// Reset registry
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when factory returns nil")
		}
	}()

	// Register a factory that returns nil
	RegisterEventByName("NilFactory", func() Event {
		return nil
	})
}
