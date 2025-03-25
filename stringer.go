package eventsourcing

import (
	"fmt"
	"strings"
)

// TypeName returns the struct name without the package path
func TypeName[T any](t T) string {
	segments := strings.Split(fmt.Sprintf("%T", t), ".")
	return segments[len(segments)-1] // Get only the struct name
}
