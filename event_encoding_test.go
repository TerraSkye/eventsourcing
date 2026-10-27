package eventsourcing

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
)

func TestEncodeDecodeEvent(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		resetRegistry(t)
		RegisterEvent(&SharedEvent{})

		data, err := EncodeEvent(&SharedEvent{ID: "1", Name: "a"})
		if err != nil {
			t.Fatalf("EncodeEvent: %v", err)
		}
		if got, want := string(data), `{"id":"1","name":"a"}`; got != want {
			t.Fatalf("EncodeEvent = %s, want %s", got, want)
		}

		ev, err := DecodeEvent("SharedEvent", data)
		if err != nil {
			t.Fatalf("DecodeEvent: %v", err)
		}
		if got, want := ev, Event(&SharedEvent{ID: "1", Name: "a"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("DecodeEvent = %#v, want %#v", got, want)
		}
	})

	t.Run("encode rejects an unregistered event", func(t *testing.T) {
		resetRegistry(t)

		if _, err := EncodeEvent(&SharedEvent{}); !errors.Is(err, ErrEventNotRegistered) {
			t.Fatalf("EncodeEvent error = %v, want ErrEventNotRegistered", err)
		}
	})

	t.Run("encode rejects an event whose type name is registered for another type", func(t *testing.T) {
		resetRegistry(t)
		RegisterEventByName("SharedEvent", func() Event { return &OtherEvent{} })

		if _, err := EncodeEvent(&SharedEvent{}); !errors.Is(err, ErrEventNotRegistered) {
			t.Fatalf("EncodeEvent error = %v, want ErrEventNotRegistered", err)
		}
	})

	t.Run("encode rejects a nil event", func(t *testing.T) {
		if _, err := EncodeEvent(nil); err == nil {
			t.Fatal("EncodeEvent(nil) succeeded, want an error")
		}
	})

	t.Run("decode rejects an unregistered name", func(t *testing.T) {
		resetRegistry(t)

		if _, err := DecodeEvent("SharedEvent", []byte(`{}`)); !errors.Is(err, ErrEventNotRegistered) {
			t.Fatalf("DecodeEvent error = %v, want ErrEventNotRegistered", err)
		}
	})

	t.Run("decode rejects malformed data", func(t *testing.T) {
		resetRegistry(t)
		RegisterEvent(&SharedEvent{})

		if _, err := DecodeEvent("SharedEvent", []byte(`{`)); err == nil {
			t.Fatal("DecodeEvent succeeded on malformed data, want an error")
		}
	})
}

func TestEncodeDecodeMetadata(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		data, err := EncodeMetadata(map[string]any{"tenant": "t1", "n": 2})
		if err != nil {
			t.Fatalf("EncodeMetadata: %v", err)
		}
		got, err := DecodeMetadata(data)
		if err != nil {
			t.Fatalf("DecodeMetadata: %v", err)
		}
		// JSON numbers decode as float64.
		if want := map[string]any{"tenant": "t1", "n": float64(2)}; !reflect.DeepEqual(got, want) {
			t.Fatalf("DecodeMetadata = %#v, want %#v", got, want)
		}
	})

	t.Run("nil encodes as an empty object", func(t *testing.T) {
		data, err := EncodeMetadata(nil)
		if err != nil {
			t.Fatalf("EncodeMetadata: %v", err)
		}
		if got := string(data); got != "{}" {
			t.Fatalf("EncodeMetadata(nil) = %s, want {}", got)
		}
	})

	for _, data := range []string{"", "null", "{}"} {
		t.Run("decodes "+strconv.Quote(data)+" as an empty map", func(t *testing.T) {
			got, err := DecodeMetadata([]byte(data))
			if err != nil {
				t.Fatalf("DecodeMetadata: %v", err)
			}
			if got == nil || len(got) != 0 {
				t.Fatalf("DecodeMetadata = %#v, want an empty, non-nil map", got)
			}
		})
	}

	t.Run("decode rejects malformed data", func(t *testing.T) {
		if _, err := DecodeMetadata([]byte(`{`)); err == nil {
			t.Fatal("DecodeMetadata succeeded on malformed data, want an error")
		}
	})
}
