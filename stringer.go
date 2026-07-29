package eventsourcing

import (
	"fmt"
	"reflect"
)

// TypeName returns t's concrete type name, without its own package path or
// any leading pointer asterisk. A generic type's name still includes its
// type argument(s), package-qualified, e.g. "Snapshot[eventsourcing.Cart]".
func TypeName[T any](t T) string {
	rt := reflect.TypeOf(t)
	if rt == nil {
		return fmt.Sprintf("%T", t) // no dynamic type to reflect on, e.g. a nil interface
	}
	for rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	return rt.Name()
}
