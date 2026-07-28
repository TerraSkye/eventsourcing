package eventsourcing

import (
	"fmt"
	"strings"
)

// TypeName returns t's concrete type name, without its package path or any
// leading pointer asterisk.
func TypeName[T any](t T) string {
	segments := strings.Split(fmt.Sprintf("%T", t), ".")
	return segments[len(segments)-1] // Get only the struct name
}
