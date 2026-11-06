package slicetest

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/terraskye/eventsourcing"
)

// Option configures a fixture.
type Option func(*options)

type options struct {
	ignore   map[string]bool
	streamID func(eventsourcing.Event) string
}

func newOptions(opts []Option) options {
	o := options{
		ignore:   map[string]bool{},
		streamID: func(ev eventsourcing.Event) string { return ev.AggregateID() },
	}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// IgnoreFields excludes exported fields by name from every event
// comparison the fixture makes: timestamps a handler sets, ids it
// generates. A name applies to every event type that has such a field.
func IgnoreFields(names ...string) Option {
	return func(o *options) {
		for _, n := range names {
			o.ignore[n] = true
		}
	}
}

// StreamID sets how Given decides which stream an event belongs to.
// Default: the event's AggregateID, which is also what
// eventsourcing.DefaultStreamNamer does for commands. Set it when the
// handler under test uses WithStreamNamer.
func StreamID(fn func(ev eventsourcing.Event) string) Option {
	return func(o *options) { o.streamID = fn }
}

// equal reports whether want and got are the same event, ignoring the
// configured fields, and a description of the first difference when not.
func (o options) equal(want, got eventsourcing.Event) (bool, string) {
	wv, gv := normalize(want, o.ignore), normalize(got, o.ignore)
	if wv.Type() != gv.Type() {
		return false, fmt.Sprintf("type %T, want %T", got, want)
	}
	if reflect.DeepEqual(wv.Interface(), gv.Interface()) {
		return true, ""
	}
	return false, fmt.Sprintf("%s\n    got:  %+v\n    want: %+v", wv.Type(), gv.Interface(), wv.Interface())
}

// normalize returns a copy of ev's underlying struct value with the
// ignored fields zeroed. Pointer events are dereferenced so that *T and T
// compare as the same type.
func normalize(ev eventsourcing.Event, ignore map[string]bool) reflect.Value {
	v := reflect.ValueOf(ev)
	for v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct || len(ignore) == 0 {
		return v
	}
	c := reflect.New(v.Type()).Elem()
	c.Set(v)
	for i := 0; i < c.NumField(); i++ {
		f := c.Type().Field(i)
		if f.IsExported() && ignore[f.Name] {
			c.Field(i).Set(reflect.Zero(f.Type))
		}
	}
	return c
}

// compare checks got against want in order and returns a report of every
// difference, or "" when they match.
func (o options) compare(want, got []eventsourcing.Event) string {
	var b strings.Builder
	n := min(len(want), len(got))
	for i := 0; i < n; i++ {
		if ok, diff := o.equal(want[i], got[i]); !ok {
			fmt.Fprintf(&b, "  event %d: %s\n", i, diff)
		}
	}
	for i := n; i < len(want); i++ {
		fmt.Fprintf(&b, "  event %d: missing, want %T %+v\n", i, want[i], deref(want[i]))
	}
	for i := n; i < len(got); i++ {
		fmt.Fprintf(&b, "  event %d: unexpected %T %+v\n", i, got[i], deref(got[i]))
	}
	return b.String()
}

func deref(ev eventsourcing.Event) any {
	v := reflect.ValueOf(ev)
	for v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	return v.Interface()
}

// typeNames lists the types of events, for failure messages.
func typeNames(events []eventsourcing.Event) string {
	names := make([]string, len(events))
	for i, ev := range events {
		names[i] = ev.EventType()
	}
	return "[" + strings.Join(names, ", ") + "]"
}
