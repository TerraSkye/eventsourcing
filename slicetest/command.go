package slicetest

import (
	"context"
	"errors"
	"testing"

	"github.com/terraskye/eventsourcing"
)

// CommandSlice tests one command handler: given past events, when a
// command is handled, then these events are recorded (or the command is
// rejected). Build one with [Command].
type CommandSlice[C eventsourcing.Command] struct {
	t       testing.TB
	log     *log
	handler eventsourcing.CommandHandler[C]
	ctx     context.Context
	since   uint64
}

// Command builds a command slice. handler constructs the handler under
// test on the store the fixture provides, typically the slice's own
// constructor:
//
//	slicetest.Command(t, func(store eventsourcing.EventStore) eventsourcing.CommandHandler[CreateTask] {
//		return createtask.NewHandler(store)
//	})
//
// Options: [IgnoreFields], [StreamID].
func Command[C eventsourcing.Command](t testing.TB, handler func(store eventsourcing.EventStore) eventsourcing.CommandHandler[C], opts ...Option) *CommandSlice[C] {
	t.Helper()
	l := newLog(t, newOptions(opts))
	return &CommandSlice[C]{t: t, log: l, handler: handler(l.store), ctx: t.Context()}
}

// Given records events as past history. Events are appended per stream,
// in the order given; the stream is the event's AggregateID unless
// [StreamID] says otherwise. Call it once, or several times, before When.
func (s *CommandSlice[C]) Given(events ...eventsourcing.Event) *CommandSlice[C] {
	s.t.Helper()
	s.since = s.log.seed(events)
	return s
}

// WithContext sets the context the command is handled with, for handlers
// that read a tenant, a user or a correlation from it. The default is the
// test's context.
func (s *CommandSlice[C]) WithContext(ctx context.Context) *CommandSlice[C] {
	s.ctx = ctx
	return s
}

// When handles cmd and returns the outcome to assert on.
func (s *CommandSlice[C]) When(cmd C) *Outcome {
	s.t.Helper()
	res, err := s.handler(s.ctx, cmd)
	return &Outcome{t: s.t, log: s.log, opts: s.log.opts, since: s.since, result: res, err: err}
}

// Store exposes the memory event store, for setups the fixture does not
// cover.
func (s *CommandSlice[C]) Store() eventsourcing.EventStore { return s.log.store }

// Outcome is what handling a command did: the events it recorded, or the
// error it returned.
type Outcome struct {
	t      testing.TB
	log    *log
	opts   options
	since  uint64
	result eventsourcing.AppendResult
	err    error
}

// ErrRejected stands for any business-rule rejection in [Outcome.Expect]:
// the handler returned an [eventsourcing.BusinessRuleViolationError],
// whatever its cause.
var ErrRejected = errors.New("slicetest: rejected by a business rule")

// Expect asserts the whole outcome in one call, so that a table test can
// put the expected error and events in its rows:
//
//	tests := []struct {
//		name    string
//		given   []eventsourcing.Event
//		when    CreateTask
//		wantErr error
//		want    []eventsourcing.Event
//	}{
//		{name: "creates", when: create, want: []eventsourcing.Event{&TaskCreated{...}}},
//		{name: "duplicate", given: []eventsourcing.Event{&TaskCreated{...}}, when: create, wantErr: slicetest.ErrRejected},
//		{name: "empty title", when: CreateTask{TaskID: id}, wantErr: createtask.ErrEmptyTitle},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			slicetest.Command(t, createtask.NewHandler).
//				Given(tt.given...).
//				When(tt.when).
//				Expect(tt.wantErr, tt.want...)
//		})
//	}
//
// A nil wantErr means the command succeeded. [ErrRejected] means it was
// rejected by any business rule. Any other wantErr must be in the
// handler's error chain ([errors.Is]); a rejection's cause is in that
// chain, so a domain error matches the rejection that wraps it. In every
// case the command must have recorded exactly events, in this order,
// across all streams: none when events is empty.
func (o *Outcome) Expect(wantErr error, events ...eventsourcing.Event) {
	o.t.Helper()
	switch {
	case wantErr == nil:
		if o.err != nil {
			o.t.Fatalf("command failed: %v", o.err)
		}
	case wantErr == ErrRejected:
		if !o.rejected() {
			o.t.Fatalf("command was not rejected by a business rule: err = %v", o.err)
		}
	default:
		if !errors.Is(o.err, wantErr) {
			o.t.Fatalf("err = %v, want it to wrap %v", o.err, wantErr)
		}
	}
	got := o.log.since(o.since)
	if diff := o.opts.compare(events, got); diff != "" {
		o.t.Fatalf("recorded events differ:\n%s  got %s, want %s", diff, typeNames(got), typeNames(events))
	}
}

func (o *Outcome) rejected() bool {
	var rejected *eventsourcing.BusinessRuleViolationError
	return errors.As(o.err, &rejected)
}

// Then asserts that the command succeeded and recorded exactly these
// events, in this order, across all streams. It is Expect(nil, events...).
func (o *Outcome) Then(events ...eventsourcing.Event) {
	o.t.Helper()
	o.Expect(nil, events...)
}

// ThenNothing asserts that the command succeeded and recorded no events.
func (o *Outcome) ThenNothing() {
	o.t.Helper()
	o.Expect(nil)
}

// ThenRejected asserts that the command was rejected by a business rule
// (the handler returned an eventsourcing.BusinessRuleViolationError) and
// recorded nothing. It is Expect(ErrRejected).
func (o *Outcome) ThenRejected() {
	o.t.Helper()
	o.Expect(ErrRejected)
}

// ThenRejectedWith asserts that the command was rejected by a business
// rule whose cause wraps target ([errors.Is]), and recorded nothing.
func (o *Outcome) ThenRejectedWith(target error) {
	o.t.Helper()
	o.ThenRejected()
	if !errors.Is(o.err, target) {
		o.t.Fatalf("rejection = %v, want it to wrap %v", o.err, target)
	}
}

// ThenError asserts that handling failed with an error that wraps target
// and is not a business-rule rejection: an infrastructure failure.
func (o *Outcome) ThenError(target error) {
	o.t.Helper()
	if o.err == nil {
		o.t.Fatal("command succeeded, want an error")
	}
	if o.rejected() {
		o.t.Fatalf("command was rejected by a business rule, want an infrastructure error: %v", o.err)
	}
	if !errors.Is(o.err, target) {
		o.t.Fatalf("err = %v, want it to wrap %v", o.err, target)
	}
}

// Events returns the events the command recorded, for custom assertions.
func (o *Outcome) Events() []eventsourcing.Event {
	o.t.Helper()
	return o.log.since(o.since)
}

// Envelopes returns the recorded events with their envelopes, for
// assertions on version, correlation or metadata.
func (o *Outcome) Envelopes() []*eventsourcing.Envelope {
	o.t.Helper()
	return o.log.envelopesSince(o.since)
}

// Err returns the handler's error, nil on success.
func (o *Outcome) Err() error { return o.err }

// Result returns the handler's AppendResult.
func (o *Outcome) Result() eventsourcing.AppendResult { return o.result }
