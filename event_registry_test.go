package eventsourcing

import (
	"encoding/json"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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

// SharedEvent mimics a normal domain event as documented in
// docs/how-to/register-events.md: a pointer type registered with RegisterEvent.
type SharedEvent struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (e *SharedEvent) EventType() string   { return "SharedEvent" }
func (e *SharedEvent) AggregateID() string { return e.ID }

func resetRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	registry = map[string]func() Event{}
	typeToNames = map[string][]string{}
	registryMu.Unlock()
}

// TestRegisterEventReturnsNewInstance asserts the invariant documented on the
// registry ("Each factory must return a new instance of a concrete Event type")
// and already enforced for RegisterEventByType in TestRegisterEventByType.
func TestRegisterEventReturnsNewInstance(t *testing.T) {
	t.Run("distinct instances", func(t *testing.T) {
		resetRegistry(t)

		RegisterEvent(&SharedEvent{})

		ev1, err := NewEventByName("SharedEvent")
		if err != nil {
			t.Fatal(err)
		}
		ev2, err := NewEventByName("SharedEvent")
		if err != nil {
			t.Fatal(err)
		}

		if ev1 == ev2 {
			t.Fatalf("factory returned the same instance twice: %p", ev1)
		}

		ev1.(*SharedEvent).Name = "first"
		if got := ev2.(*SharedEvent).Name; got != "" {
			t.Fatalf("mutating one instance leaked into the other: got %q, want %q", got, "")
		}
	})

	// This reproduces the exact decode path used by every persistent event
	// store, e.g. eventstore/file/filestorage.go:231-239 and
	// eventstore/postgres/eventstore.go:268.
	t.Run("decoding two stored events yields independent values", func(t *testing.T) {
		resetRegistry(t)

		RegisterEvent(&SharedEvent{})

		stored := [][]byte{
			[]byte(`{"id":"agg-1","name":"first"}`),
			[]byte(`{"id":"agg-1","name":"second"}`),
		}

		decoded := make([]Event, 0, len(stored))
		for _, data := range stored {
			ev, err := NewEventByName("SharedEvent")
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &ev); err != nil {
				t.Fatal(err)
			}
			decoded = append(decoded, ev)
		}

		want := []string{"first", "second"}
		for i, ev := range decoded {
			if got := ev.(*SharedEvent).Name; got != want[i] {
				t.Errorf("decoded[%d].Name = %q, want %q", i, got, want[i])
			}
		}
	})
}

// The tests in this file pin the serialize -> deserialize contract that every
// persistent EventStore and EventBus implementation shares: an event is
// json.Marshal'ed on the way in, and on the way out a fresh instance is built
// from the registry with NewEventByName and json.Unmarshal'ed into. See
// eventstore/file/filestorage.go, eventstore/postgres/eventstore.go and
// eventstore/kurrentdb/eventstore.go, which all decode this way.
//
// Each backend has its own roundtrip test that drives a real Save/LoadStream
// through this contract; these tests cover the parts that are the same
// everywhere and need no infrastructure to exercise.

// Money is a value object nested inside orderPlaced.
type Money struct {
	Amount   int64  `json:"amount"`
	Currency string `json:"currency"`
}

// LineItem nests Money one level deeper and is carried in a slice.
type LineItem struct {
	SKU   string   `json:"sku"`
	Qty   int      `json:"qty"`
	Price Money    `json:"price"`
	Notes []string `json:"notes"`
}

// ShippingAddress is reached through a pointer field, so a roundtrip has to
// cover both the populated and the nil case.
type ShippingAddress struct {
	Street  string `json:"street"`
	ZIP     string `json:"zip"`
	Country string `json:"country"`
}

// orderPlaced deliberately mixes every kind of field a domain event
// realistically carries — nested structs, slices of structs, pointers, maps,
// a uuid.UUID and a time.Time — so a single roundtrip exercises all of them.
type orderPlaced struct {
	OrderID   string            `json:"order_id"`
	TraceID   uuid.UUID         `json:"trace_id"`
	PlacedAt  time.Time         `json:"placed_at"`
	Total     Money             `json:"total"`
	Items     []LineItem        `json:"items"`
	ShipTo    *ShippingAddress  `json:"ship_to"`
	BillTo    *ShippingAddress  `json:"bill_to"`
	Labels    map[string]string `json:"labels"`
	Counts    map[string]int    `json:"counts"`
	Discount  float64           `json:"discount"`
	Expedited bool              `json:"expedited"`
}

func (e *orderPlaced) AggregateID() string { return e.OrderID }
func (e *orderPlaced) EventType() string   { return "orderPlaced" }

// newOrderPlaced returns a fully populated event. Every field is non-zero
// except BillTo, which stays nil so the nil-pointer case is covered too.
func newOrderPlaced() *orderPlaced {
	return &orderPlaced{
		OrderID:  "order-42",
		TraceID:  uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8"),
		PlacedAt: time.Date(2024, 3, 1, 12, 34, 56, 123456789, time.UTC),
		Total:    Money{Amount: 4999, Currency: "EUR"},
		Items: []LineItem{
			{SKU: "WIDGET-1", Qty: 2, Price: Money{Amount: 1999, Currency: "EUR"}, Notes: []string{"gift wrap"}},
			{SKU: "WIDGET-2", Qty: 1, Price: Money{Amount: 1001, Currency: "EUR"}},
		},
		ShipTo:    &ShippingAddress{Street: "Keizersgracht 1", ZIP: "1015 CJ", Country: "NL"},
		BillTo:    nil,
		Labels:    map[string]string{"channel": "web", "campaign": "spring"},
		Counts:    map[string]int{"widgets": 3},
		Discount:  12.5,
		Expedited: true,
	}
}

// roundtrip runs event through the exact path a persistent store uses.
func roundtrip(t *testing.T, event Event) Event {
	t.Helper()

	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal %T: %v", event, err)
	}

	decoded, err := NewEventByName(event.EventType())
	if err != nil {
		t.Fatalf("NewEventByName(%q): %v", event.EventType(), err)
	}
	if err := json.Unmarshal(data, decoded); err != nil {
		t.Fatalf("unmarshal %q: %v", event.EventType(), err)
	}
	return decoded
}

// TestSerializationRoundtrip_NestedTypes asserts that an event carrying
// nested structs, slices of structs, pointer fields (set and nil) and maps
// comes back out of the registry byte-for-byte equal to what went in.
func TestSerializationRoundtrip_NestedTypes(t *testing.T) {
	resetRegistry(t)
	RegisterEvent(&orderPlaced{})

	want := newOrderPlaced()
	got, ok := roundtrip(t, want).(*orderPlaced)
	if !ok {
		t.Fatalf("decoded event is not *orderPlaced")
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip changed the event:\n got: %#v\nwant: %#v", got, want)
	}

	// Spot-check the parts DeepEqual would also accept as equal if the whole
	// substructure were missing, so a regression names itself in the output.
	if len(got.Items) != len(want.Items) {
		t.Errorf("Items = %d, want %d", len(got.Items), len(want.Items))
	}
	if got.ShipTo == nil || *got.ShipTo != *want.ShipTo {
		t.Errorf("ShipTo = %#v, want %#v", got.ShipTo, want.ShipTo)
	}
	if got.BillTo != nil {
		t.Errorf("BillTo = %#v, want nil pointer to survive as nil", got.BillTo)
	}
	if got.Items[1].Notes != nil {
		t.Errorf("Items[1].Notes = %#v, want a nil slice to survive as nil", got.Items[1].Notes)
	}
}

// TestSerializationRoundtrip_UUIDPrecision asserts that uuid.UUID fields
// survive as the same 16 bytes, including the zero UUID.
func TestSerializationRoundtrip_UUIDPrecision(t *testing.T) {
	resetRegistry(t)
	RegisterEvent(&orderPlaced{})

	t.Run("populated", func(t *testing.T) {
		want := newOrderPlaced()
		got := roundtrip(t, want).(*orderPlaced)

		if got.TraceID != want.TraceID {
			t.Fatalf("TraceID = %v, want %v", got.TraceID, want.TraceID)
		}
		if got.TraceID.String() != want.TraceID.String() {
			t.Fatalf("TraceID.String() = %q, want %q", got.TraceID, want.TraceID)
		}
	})

	t.Run("zero value", func(t *testing.T) {
		want := newOrderPlaced()
		want.TraceID = uuid.Nil
		got := roundtrip(t, want).(*orderPlaced)

		if got.TraceID != uuid.Nil {
			t.Fatalf("TraceID = %v, want uuid.Nil", got.TraceID)
		}
	})

	t.Run("random values", func(t *testing.T) {
		for range 100 {
			want := newOrderPlaced()
			want.TraceID = uuid.New()
			got := roundtrip(t, want).(*orderPlaced)

			if got.TraceID != want.TraceID {
				t.Fatalf("TraceID = %v, want %v", got.TraceID, want.TraceID)
			}
		}
	})
}

// TestSerializationRoundtrip_TimePrecision pins how time.Time survives the
// JSON hop: encoding/time uses RFC 3339 with nanoseconds, which keeps the
// instant exactly but drops both the monotonic clock reading and the zone
// *name*. Callers comparing timestamps read back from a store must therefore
// use Time.Equal rather than ==.
func TestSerializationRoundtrip_TimePrecision(t *testing.T) {
	resetRegistry(t)
	RegisterEvent(&orderPlaced{})

	t.Run("nanosecond precision is preserved", func(t *testing.T) {
		want := newOrderPlaced()
		got := roundtrip(t, want).(*orderPlaced)

		if !got.PlacedAt.Equal(want.PlacedAt) {
			t.Fatalf("PlacedAt = %v, want %v", got.PlacedAt, want.PlacedAt)
		}
		if got.PlacedAt.Nanosecond() != 123456789 {
			t.Fatalf("PlacedAt.Nanosecond() = %d, want 123456789", got.PlacedAt.Nanosecond())
		}
		if got.PlacedAt != want.PlacedAt {
			t.Fatalf("PlacedAt is not identical to the original: %v != %v", got.PlacedAt, want.PlacedAt)
		}
	})

	t.Run("sub-second values that end in zeros", func(t *testing.T) {
		// RFC 3339 drops trailing zeros in the fractional second, so
		// ".100000000" is written as ".1"; the decoded instant must still
		// match.
		want := newOrderPlaced()
		want.PlacedAt = time.Date(2024, 3, 1, 12, 34, 56, 100000000, time.UTC)
		got := roundtrip(t, want).(*orderPlaced)

		if !got.PlacedAt.Equal(want.PlacedAt) {
			t.Fatalf("PlacedAt = %v, want %v", got.PlacedAt, want.PlacedAt)
		}
	})

	t.Run("monotonic clock reading is dropped", func(t *testing.T) {
		want := newOrderPlaced()
		want.PlacedAt = time.Now() // carries a monotonic reading
		got := roundtrip(t, want).(*orderPlaced)

		if !got.PlacedAt.Equal(want.PlacedAt) {
			t.Fatalf("PlacedAt = %v, want %v", got.PlacedAt, want.PlacedAt)
		}
		if got.PlacedAt == want.PlacedAt {
			t.Fatal("expected == to fail after the monotonic reading is stripped; if this now passes, the note about using Time.Equal can be relaxed")
		}
		// Stripping the monotonic reading is the only difference.
		if got.PlacedAt != want.PlacedAt.Round(0) {
			t.Fatalf("PlacedAt = %v, want %v", got.PlacedAt, want.PlacedAt.Round(0))
		}
	})

	t.Run("zone offset survives but the zone name does not", func(t *testing.T) {
		want := newOrderPlaced()
		want.PlacedAt = time.Date(2024, 3, 1, 12, 0, 0, 0, time.FixedZone("CET", 2*60*60))
		got := roundtrip(t, want).(*orderPlaced)

		if !got.PlacedAt.Equal(want.PlacedAt) {
			t.Fatalf("PlacedAt = %v, want %v", got.PlacedAt, want.PlacedAt)
		}
		_, wantOffset := want.PlacedAt.Zone()
		gotName, gotOffset := got.PlacedAt.Zone()
		if gotOffset != wantOffset {
			t.Fatalf("zone offset = %d, want %d", gotOffset, wantOffset)
		}
		if gotName == "CET" {
			t.Fatal("expected the zone name to be lost across JSON; if it now survives, this expectation can be tightened")
		}
	})

	t.Run("zero time", func(t *testing.T) {
		want := newOrderPlaced()
		want.PlacedAt = time.Time{}
		got := roundtrip(t, want).(*orderPlaced)

		if !got.PlacedAt.IsZero() {
			t.Fatalf("PlacedAt = %v, want the zero time", got.PlacedAt)
		}
	})
}

// inventoryChanged stands in for an event whose schema has moved on since the
// stored events were written: "reason" and "audit" have since been removed,
// and Location has since been added.
type inventoryChanged struct {
	SKU      string  `json:"sku"`
	Delta    int     `json:"delta"`
	Batches  []batch `json:"batches"`
	Location string  `json:"location"`
}

type batch struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

func (e *inventoryChanged) AggregateID() string { return e.SKU }
func (e *inventoryChanged) EventType() string   { return "inventoryChanged" }

// TestSerializationRoundtrip_UnknownFields asserts that decoding an event
// written by an older build does not fail: fields that no longer exist on the
// struct are ignored (at the top level, nested in an object and inside array
// elements), and fields added since are left at their zero value. Without
// this, any additive schema change would break replay of existing streams.
func TestSerializationRoundtrip_UnknownFields(t *testing.T) {
	resetRegistry(t)
	RegisterEvent(&inventoryChanged{})

	// Exactly the bytes an older build would have persisted.
	stored := []byte(`{
		"sku": "WIDGET-1",
		"delta": -3,
		"reason": "shrinkage",
		"audit": {"user": "ops", "at": "2024-03-01T12:00:00Z"},
		"batches": [{"id": "b1", "count": 2, "expired": false}]
	}`)

	decoded, err := NewEventByName("inventoryChanged")
	if err != nil {
		t.Fatalf("NewEventByName: %v", err)
	}
	if err := json.Unmarshal(stored, decoded); err != nil {
		t.Fatalf("decoding an event with unknown fields must not fail: %v", err)
	}

	got, ok := decoded.(*inventoryChanged)
	if !ok {
		t.Fatalf("decoded event is %T, want *inventoryChanged", decoded)
	}

	want := &inventoryChanged{
		SKU:     "WIDGET-1",
		Delta:   -3,
		Batches: []batch{{ID: "b1", Count: 2}},
		// Location was added after this event was written.
		Location: "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded = %#v, want %#v", got, want)
	}
}

// TestSerializationRoundtrip_MetadataFidelity pins what Envelope.Metadata
// survives. Because it is a map[string]any it is decoded generically, so —
// unlike a struct field — the Go type of every value is decided by JSON, not
// by what the caller put in. Every store shares this behaviour.
func TestSerializationRoundtrip_MetadataFidelity(t *testing.T) {
	// A value above 2^53 cannot be held exactly by the float64 that JSON
	// numbers decode into.
	const beyondFloat64 int64 = 1<<53 + 1

	meta := map[string]any{
		"user":     "alice",
		"retries":  3,
		"ratio":    1.5,
		"enabled":  true,
		"absent":   nil,
		"trace":    map[string]any{"span": "abc", "depth": 2},
		"tags":     []string{"a", "b"},
		"sequence": beyondFloat64,
	}

	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}

	if got["user"] != "alice" {
		t.Errorf(`metadata["user"] = %#v, want "alice"`, got["user"])
	}
	if got["enabled"] != true {
		t.Errorf(`metadata["enabled"] = %#v, want true`, got["enabled"])
	}
	if got["absent"] != nil {
		t.Errorf(`metadata["absent"] = %#v, want nil`, got["absent"])
	}
	if got["ratio"] != 1.5 {
		t.Errorf(`metadata["ratio"] = %#v, want 1.5`, got["ratio"])
	}

	// An int goes in, a float64 comes out. Callers must type-assert to
	// float64, not to the type they stored.
	if _, isInt := got["retries"].(int); isInt {
		t.Error(`metadata["retries"] decoded as int; every JSON number decodes as float64`)
	}
	if v, ok := got["retries"].(float64); !ok || v != 3 {
		t.Errorf(`metadata["retries"] = %#v (%[1]T), want float64(3)`, got["retries"])
	}

	// The float64 hop is lossy beyond 2^53; anything that must survive
	// exactly belongs in a typed event field, not in Metadata.
	if v, ok := got["sequence"].(float64); !ok || int64(v) == beyondFloat64 {
		t.Errorf(`metadata["sequence"] = %#v, want a float64 that has lost precision against %d`, got["sequence"], beyondFloat64)
	}

	// Nested objects stay generic all the way down.
	trace, ok := got["trace"].(map[string]any)
	if !ok {
		t.Fatalf(`metadata["trace"] = %#v (%[1]T), want map[string]any`, got["trace"])
	}
	if trace["span"] != "abc" {
		t.Errorf(`metadata["trace"]["span"] = %#v, want "abc"`, trace["span"])
	}
	if v, ok := trace["depth"].(float64); !ok || v != 2 {
		t.Errorf(`metadata["trace"]["depth"] = %#v (%[1]T), want float64(2)`, trace["depth"])
	}

	// A []string goes in, an []any comes out.
	tags, ok := got["tags"].([]any)
	if !ok {
		t.Fatalf(`metadata["tags"] = %#v (%[1]T), want []any`, got["tags"])
	}
	if len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf(`metadata["tags"] = %#v, want ["a" "b"]`, tags)
	}

	t.Run("nil map", func(t *testing.T) {
		data, err := json.Marshal(map[string]any(nil))
		if err != nil {
			t.Fatalf("marshal nil metadata: %v", err)
		}
		if string(data) != "null" {
			t.Fatalf("nil metadata encoded as %s, want null", data)
		}
		var got map[string]any
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshal nil metadata: %v", err)
		}
		if got != nil {
			t.Fatalf("nil metadata decoded as %#v, want nil", got)
		}
	})
}

// TestSerializationRoundtrip_DecodeTargetForms asserts that the two ways the
// stores call json.Unmarshal are equivalent: the file store passes a pointer
// to the Event interface variable, postgres and kurrentdb pass the interface
// value itself. Both must fill the same underlying struct.
func TestSerializationRoundtrip_DecodeTargetForms(t *testing.T) {
	resetRegistry(t)
	RegisterEvent(&orderPlaced{})

	data, err := json.Marshal(newOrderPlaced())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	viaValue, err := NewEventByName("orderPlaced")
	if err != nil {
		t.Fatalf("NewEventByName: %v", err)
	}
	if err := json.Unmarshal(data, viaValue); err != nil {
		t.Fatalf("unmarshal into the interface value: %v", err)
	}

	viaPointer, err := NewEventByName("orderPlaced")
	if err != nil {
		t.Fatalf("NewEventByName: %v", err)
	}
	if err := json.Unmarshal(data, &viaPointer); err != nil {
		t.Fatalf("unmarshal into a pointer to the interface: %v", err)
	}

	if !reflect.DeepEqual(viaValue, viaPointer) {
		t.Fatalf("the two decode forms disagree:\n value: %#v\npointer: %#v", viaValue, viaPointer)
	}
	if !reflect.DeepEqual(viaValue, newOrderPlaced()) {
		t.Fatalf("decoded = %#v, want %#v", viaValue, newOrderPlaced())
	}
}

// valueReceiverEvent is registered by a factory that returns a non-pointer
// Event, which is what RegisterEvent's pointer constraint exists to prevent.
type valueReceiverEvent struct {
	N int `json:"n"`
}

func (e valueReceiverEvent) AggregateID() string { return "value" }
func (e valueReceiverEvent) EventType() string   { return "valueReceiverEvent" }

// TestSerializationRoundtrip_ValueFactoryCannotDecode documents why a
// registered factory must return a pointer: a factory returning a value gives
// json.Unmarshal an unaddressable target, so the decode fails outright rather
// than silently producing a zero-valued event. The failure is loud in both
// call forms the stores use.
func TestSerializationRoundtrip_ValueFactoryCannotDecode(t *testing.T) {
	resetRegistry(t)
	RegisterEventByType(func() Event { return valueReceiverEvent{} })

	data := []byte(`{"n":42}`)

	t.Run("interface value", func(t *testing.T) {
		decoded, err := NewEventByName("valueReceiverEvent")
		if err != nil {
			t.Fatalf("NewEventByName: %v", err)
		}
		if err := json.Unmarshal(data, decoded); err == nil {
			t.Fatalf("expected an error decoding into a value-typed event, got %#v", decoded)
		}
		if got := decoded.(valueReceiverEvent).N; got != 0 {
			t.Fatalf("N = %d, want 0", got)
		}
	})

	t.Run("pointer to interface", func(t *testing.T) {
		decoded, err := NewEventByName("valueReceiverEvent")
		if err != nil {
			t.Fatalf("NewEventByName: %v", err)
		}
		if err := json.Unmarshal(data, &decoded); err == nil {
			t.Fatalf("expected an error decoding into a value-typed event, got %#v", decoded)
		}
	})
}
