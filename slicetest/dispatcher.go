package slicetest

import (
	"context"
	"sync"

	"github.com/terraskye/eventsourcing"
)

// RecordingDispatcher is an eventsourcing.Dispatcher that records every
// command it receives and answers each with a scripted result. It stands
// in for the command bus when a Work function or an HTTP handler is tested
// on its own.
//
//	bus := slicetest.NewRecordingDispatcher()
//	bus.Reject("CloseConversation", eventsourcing.NewBusinessRuleViolation(errors.New("already closed")))
//	err := closer.Work(ctx, todo)
//	cmds := bus.Dispatched()
type RecordingDispatcher struct {
	mu       sync.Mutex
	commands []eventsourcing.Command
	results  map[string]error
	fallback func(eventsourcing.Command) (eventsourcing.AppendResult, error)
}

// NewRecordingDispatcher returns a dispatcher that accepts every command.
func NewRecordingDispatcher() *RecordingDispatcher {
	return &RecordingDispatcher{results: map[string]error{}}
}

// Reject makes every command of the given CommandType fail with err.
// A nil err clears it.
func (d *RecordingDispatcher) Reject(commandType string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.results, commandType)
		return
	}
	d.results[commandType] = err
}

// Respond sets a function that answers every command without a scripted
// rejection, for tests that need a result or a per-command decision.
func (d *RecordingDispatcher) Respond(fn func(eventsourcing.Command) (eventsourcing.AppendResult, error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fallback = fn
}

// Dispatch implements eventsourcing.Dispatcher.
func (d *RecordingDispatcher) Dispatch(_ context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.commands = append(d.commands, cmd)
	if err, ok := d.results[cmd.CommandType()]; ok {
		return eventsourcing.AppendResult{}, err
	}
	if d.fallback != nil {
		return d.fallback(cmd)
	}
	return eventsourcing.AppendResult{Successful: true}, nil
}

// Dispatched returns the commands received so far, in order.
func (d *RecordingDispatcher) Dispatched() []eventsourcing.Command {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]eventsourcing.Command(nil), d.commands...)
}

// Reset forgets the recorded commands; scripted results stay.
func (d *RecordingDispatcher) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.commands = nil
}
