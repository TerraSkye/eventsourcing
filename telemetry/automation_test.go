package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/memory"
	"go.opentelemetry.io/otel/codes"
)

// newAutomationName returns an automation name no other test uses, so the
// test can pick its own records out of the shared recorders.
func newAutomationName() string {
	return "otel-automation-" + uuid.NewString()
}

// forAutomation returns the records for name, in the order they were
// recorded.
func forAutomation(records []histRecord, name string) []histRecord {
	var out []histRecord
	for _, rec := range records {
		if v, ok := rec.attrs.Value(AttrAutomationName); ok && v.AsString() == name {
			out = append(out, rec)
		}
	}
	return out
}

// items is a plain todo list of string IDs.
type items struct {
	mu      sync.Mutex
	pending []string
}

func (l *items) Claim(_ context.Context, req automation.ClaimRequest) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := min(req.MaxPerKey, len(l.pending))
	out := l.pending[:n:n]
	l.pending = l.pending[n:]
	return out, nil
}

// quietLogger discards the runner's log.
var quietLogger = slog.New(slog.DiscardHandler)

type liveDep struct{}

func (liveDep) Name() string                         { return "todo" }
func (liveDep) IsLive(context.Context) (bool, error) { return true, nil }

// TestAutomationTelemetry_Runner runs an automation over three items with
// its work wrapped, one succeeding, one deferring and one failing, and
// checks the work spans, the duration per result, and the observed phase
// and item counts.
func TestAutomationTelemetry_Runner(t *testing.T) {
	name := newAutomationName()
	list := &items{pending: []string{"ok", "bad", "later"}}
	work := WithWorkTelemetry(name, func(_ context.Context, id string) error {
		switch id {
		case "later":
			return automation.Deferred(time.Now().Add(time.Hour))
		case "bad":
			return errors.New("carrier down")
		}
		return nil
	})
	r := automation.NewRunner(automation.Definition[string]{
		Name:         name,
		Projection:   liveDep{},
		Store:        WithAutomationStoreTelemetry(memory.NewAutomationStore()),
		TodoList:     list,
		Work:         work,
		BatchSize:    1,
		PollInterval: 10 * time.Millisecond,
		Logger:       quietLogger,
	})
	reg, err := ObserveAutomation(r)
	if err != nil {
		t.Fatalf("ObserveAutomation: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	before := spanNameRecorder.len()
	// The deferral comes last: it pauses the unsegmented list for an hour,
	// so RunOnce stops there and nothing after it would be worked.
	if _, err := r.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	var spans, events []string
	for _, s := range spanNameRecorder.spansSince(before) {
		if attrString(s.attrs, AttrAutomationName) == name {
			spans = append(spans, s.name)
			for _, ev := range s.events {
				events = append(events, s.name+": "+ev.name)
			}
		}
	}
	if len(spans) == 0 || spans[0] != "process item" {
		t.Errorf("spans for the automation = %v, want process item spans", spans)
	}
	// The runner defers the segment through the store after the work
	// returned, outside the work's span; only the work's own event records
	// the deferral.
	if want := []string{"process item: segment_deferred"}; !slices.Equal(events, want) {
		t.Errorf("span events = %v, want %v", events, want)
	}

	var results []string
	for _, rec := range forAutomation(durationRecorder.recordsFor("eventsourcing.automation.work.duration"), name) {
		results = append(results, attrString(rec.attrs, AttrResult))
		if attrString(rec.attrs, AttrResult) == "failure" && attrString(rec.attrs, AttrErrorType) != "_OTHER" {
			t.Errorf("failure without error.type: %v", rec.attrs)
		}
	}
	if want := []string{"success", "failure", "deferred"}; !slices.Equal(results, want) {
		t.Errorf("work results = %v, want %v", results, want)
	}

	observed := map[string]float64{}
	for _, rec := range forAutomation(durationRecorder.collect(t.Context()), name) {
		key := rec.name
		if p := attrString(rec.attrs, AttrAutomationPhase); p != "" {
			key += "/" + p
		}
		if res := attrString(rec.attrs, AttrResult); res != "" {
			key += "/" + res
		}
		observed[key] = rec.value
	}
	want := map[string]float64{
		"eventsourcing.automation.items/success":  1,
		"eventsourcing.automation.items/deferred": 1,
		"eventsourcing.automation.items/failure":  1,
		"eventsourcing.automation.phase/idle":     1,
		"eventsourcing.automation.phase/working":  0,
		"eventsourcing.automation.phase/paused":   0,
		"eventsourcing.automation.phase/stalled":  0,
	}
	for k, v := range want {
		if got, ok := observed[k]; !ok || got != v {
			t.Errorf("observed %s = %v (present %v), want %v", k, got, ok, v)
		}
	}
}

// TestWithWorkTelemetry_Results checks the result label for each kind of
// return, the error type on a failure, and that the error passes through.
// Work that returns the cancellation of its context is "canceled", as the
// run is stopping; a lease deadline it ran past, or a cancellation it
// returns while its context is live, is a failure like any other.
func TestWithWorkTelemetry_Results(t *testing.T) {
	name := newAutomationName()
	boom := errors.New("boom")
	work := WithWorkTelemetry(name, func(ctx context.Context, ret error) error {
		if errors.Is(ret, errFromContext) {
			return fmt.Errorf("send: %w", ctx.Err())
		}
		return ret
	})
	live := t.Context()
	canceled, cancel := context.WithCancel(live)
	cancel()
	expired, cancel := context.WithDeadline(live, time.Now().Add(-time.Second))
	defer cancel()
	for _, tc := range []struct {
		ctx context.Context
		ret error
	}{
		{live, nil},
		{live, automation.Deferred(time.Time{})},
		{live, boom},
		{live, context.DeadlineExceeded},
		{live, context.Canceled},
		{canceled, errFromContext},
		{expired, errFromContext},
		{canceled, boom},
	} {
		err := work(tc.ctx, tc.ret)
		if tc.ret == errFromContext {
			if !errors.Is(err, tc.ctx.Err()) {
				t.Fatalf("work(ctx.Err()) = %v, want it passed through", err)
			}
		} else if !errors.Is(err, tc.ret) && err != tc.ret {
			t.Fatalf("work(%v) = %v, want it passed through", tc.ret, err)
		}
	}
	var got []string
	for _, rec := range forAutomation(durationRecorder.recordsFor("eventsourcing.automation.work.duration"), name) {
		got = append(got, attrString(rec.attrs, AttrResult)+"/"+attrString(rec.attrs, AttrErrorType))
	}
	want := []string{
		"success/", "deferred/", "failure/_OTHER", "failure/deadline_exceeded", "failure/canceled",
		"canceled/", "failure/deadline_exceeded", "failure/_OTHER",
	}
	if !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v", got, want)
	}
}

// errFromContext tells the work of TestWithWorkTelemetry_Results to return
// its context's error.
var errFromContext = errors.New("return ctx.Err()")

// TestWithWorkTelemetry_DeferredEvent checks that a deferral adds the
// segment_deferred event to the work's span, with the time deferred until
// when the deferral names one.
func TestWithWorkTelemetry_DeferredEvent(t *testing.T) {
	name := newAutomationName()
	until := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	work := WithWorkTelemetry(name, func(_ context.Context, ret error) error { return ret })
	before := spanNameRecorder.len()
	for _, ret := range []error{automation.Deferred(until), automation.Deferred(time.Time{}), nil} {
		_ = work(t.Context(), ret)
	}
	var got []string
	for _, s := range spanNameRecorder.spansSince(before) {
		if attrString(s.attrs, AttrAutomationName) != name {
			continue
		}
		var events []string
		for _, ev := range s.events {
			events = append(events, ev.name+"/"+attrString(ev.attrs, "eventsourcing.automation.deferred_until"))
		}
		got = append(got, fmt.Sprint(events))
	}
	want := []string{"[segment_deferred/2026-11-08T12:00:00Z]", "[segment_deferred/]", "[]"}
	if !slices.Equal(got, want) {
		t.Errorf("events per span = %v, want %v", got, want)
	}
}

// TestWithWorkTelemetry_Operation checks the span name options.
func TestWithWorkTelemetry_Operation(t *testing.T) {
	name := newAutomationName()
	before := spanNameRecorder.len()
	work := WithWorkTelemetry(name, func(context.Context, string) error { return nil }, WithOperation("ship order"))
	if err := work(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := spanNameRecorder.since(before); !slices.Contains(got, "ship order") {
		t.Errorf("spans = %v, want ship order", got)
	}
}

// shipOrders is the automation of ExampleWithWorkTelemetry: a type next to
// its todo list with a Definition method, as automation.Definition
// describes.
type shipOrders struct {
	list *items
	todo automation.Dependency
}

func newShipOrders(list *items, todo automation.Dependency) *shipOrders {
	return &shipOrders{list: list, todo: todo}
}

func (a *shipOrders) Definition() automation.Definition[string] {
	return automation.Definition[string]{Name: "ship-orders", TodoList: a.list, Projection: a.todo, Work: a.Work}
}

func (a *shipOrders) Work(context.Context, string) error { return nil }

// ExampleWithWorkTelemetry wraps an automation's work in main, after the
// automation described itself and before the runner is created.
func ExampleWithWorkTelemetry() {
	ship := newShipOrders(&items{}, liveDep{}).Definition()
	ship.Work = WithWorkTelemetry(ship.Name, ship.Work)
	ship.Store = memory.NewAutomationStore() // postgres.NewAutomationStore(pool) to share leases
	runner := automation.NewRunner(ship)
	_ = runner.Run // go runner.Run(ctx)
}

// TestWithWorkTelemetry_SpanStatus covers the span status: a success, a
// deferral and the cancellation of a stopping run leave it unset, as the
// OpenTelemetry Trace API asks of instrumentation, and any other error
// sets it to Error.
func TestWithWorkTelemetry_SpanStatus(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	tests := map[string]struct {
		ctx  context.Context
		err  error
		want codes.Code
	}{
		"success":  {t.Context(), nil, codes.Unset},
		"deferred": {t.Context(), automation.Deferred(time.Time{}), codes.Unset},
		"canceled": {canceled, context.Canceled, codes.Unset},
		"failure":  {t.Context(), errors.New("boom"), codes.Error},
	}
	work := WithWorkTelemetry(newAutomationName(), func(_ context.Context, ret error) error { return ret })
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			before := spanNameRecorder.len()
			_ = work(tt.ctx, tt.err)
			spans := spanNameRecorder.spansSince(before)
			if len(spans) != 1 || spans[0].status != tt.want {
				t.Errorf("spans = %+v, want one with status %v", spans, tt.want)
			}
		})
	}
}
