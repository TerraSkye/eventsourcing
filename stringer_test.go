package eventsourcing

import (
	"strings"
	"testing"
)

type genericWrapper[T any] struct {
	Value T
}

type payload struct{}

// TestTypeName_GenericType is a regression test for GitHub issue #58:
// TypeName split fmt.Sprintf("%T", t) on every ".", but a generic
// instantiation's %T form is fully package-qualified on its type argument
// too (e.g. "eventsourcing.genericWrapper[github.com/terraskye/eventsourcing.payload]"),
// so the naive last-segment split returned a mangled fragment of the type
// argument ("payload]") with the actual struct name discarded entirely.
func TestTypeName_GenericType(t *testing.T) {
	got := TypeName(genericWrapper[payload]{})
	if !strings.HasPrefix(got, "genericWrapper") {
		t.Fatalf("TypeName(genericWrapper[payload]{}) = %q, want a name starting with %q", got, "genericWrapper")
	}
}

func TestTypeName_PlainStruct(t *testing.T) {
	type Bar struct{}
	if got := TypeName(Bar{}); got != "Bar" {
		t.Errorf("TypeName(Bar{}) = %q, want %q", got, "Bar")
	}
}

func TestTypeName_Pointer(t *testing.T) {
	type Bar struct{}
	if got := TypeName(&Bar{}); got != "Bar" {
		t.Errorf("TypeName(&Bar{}) = %q, want %q", got, "Bar")
	}
}
