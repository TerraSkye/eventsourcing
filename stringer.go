package eventsourcing

import (
	"fmt"
	"strings"
)

// TypeName returns t's concrete type name, without its own package path or
// any leading pointer asterisk. A generic type's name still includes its
// type argument(s), package-qualified, e.g. "Snapshot[eventsourcing.Cart]".
func TypeName[T any](t T) string {
	s := fmt.Sprintf("%T", t)

	// A generic instantiation's %T form is package-qualified inside the
	// brackets too, e.g. "eventsourcing.Snapshot[eventsourcing.Cart]" - and
	// that inner qualifier can itself contain dots (a full import path), so
	// only the portion before the first '[' is eligible for stripping down
	// to its own package-qualifying dot.
	name, suffix := s, ""
	if i := strings.IndexByte(s, '['); i >= 0 {
		name, suffix = s[:i], s[i:]
	}

	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		name = name[i+1:]
	}

	return name + suffix
}
