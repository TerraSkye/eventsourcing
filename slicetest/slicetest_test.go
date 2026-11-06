package slicetest_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/slicetest"
)

// A small counter domain: three slices, enough to cross a scenario.

type Incremented struct {
	CounterID string
	By        int
	At        time.Time // set by the decider: the kind of field IgnoreFields is for
}

func (e Incremented) AggregateID() string { return e.CounterID }
func (e Incremented) EventType() string   { return "Incremented" }

type CounterReset struct{ CounterID string }

func (e CounterReset) AggregateID() string { return e.CounterID }
func (e CounterReset) EventType() string   { return "CounterReset" }

func init() {
	eventsourcing.RegisterEvent(&Incremented{})
	eventsourcing.RegisterEvent(&CounterReset{})
}

type Increment struct {
	CounterID string
	By        int
}

func (c Increment) AggregateID() string { return c.CounterID }
func (c Increment) CommandType() string { return "Increment" }

type ResetCounter struct{ CounterID string }

func (c ResetCounter) AggregateID() string { return c.CounterID }
func (c ResetCounter) CommandType() string { return "ResetCounter" }

var errNonPositive = errors.New("increment must be positive")

type counter struct{ Total int }

func evolve(s counter, env *eventsourcing.Envelope) counter {
	switch e := env.Event.(type) {
	case *Incremented:
		s.Total += e.By
	case *CounterReset:
		s.Total = 0
	}
	return s
}

func incrementHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[Increment] {
	return eventsourcing.NewCommandHandler(store,
		func() counter { return counter{} }, evolve,
		func(s counter, cmd Increment) ([]eventsourcing.Event, error) {
			if cmd.By <= 0 {
				return nil, errNonPositive
			}
			return []eventsourcing.Event{&Incremented{CounterID: cmd.CounterID, By: cmd.By, At: time.Now()}}, nil
		})
}

func resetHandler(store eventsourcing.EventStore) eventsourcing.CommandHandler[ResetCounter] {
	return eventsourcing.NewCommandHandler(store,
		func() counter { return counter{} }, evolve,
		func(s counter, cmd ResetCounter) ([]eventsourcing.Event, error) {
			if s.Total == 0 {
				return nil, errors.New("nothing to reset")
			}
			return []eventsourcing.Event{&CounterReset{CounterID: cmd.CounterID}}, nil
		})
}

// totals is the view: a read model, and a todo list of counters that
// reached the limit and must be reset.
type totals struct {
	mu     sync.Mutex
	totals map[string]int
	due    map[string]time.Time // counter → claimed until
	limit  int
}

func newTotals(limit int) *totals {
	return &totals{totals: map[string]int{}, due: map[string]time.Time{}, limit: limit}
}

func (v *totals) projection() projection.Definition {
	group := eventsourcing.NewEventGroupProcessor(
		eventsourcing.OnEvent(v.onIncremented),
		eventsourcing.OnEvent(v.onReset),
	)
	return projection.Definition{Name: "totals", Handler: group, EventTypes: group.StreamFilter()}
}

func (v *totals) onIncremented(_ context.Context, e *Incremented) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.totals[e.CounterID] += e.By
	if v.totals[e.CounterID] >= v.limit {
		if _, ok := v.due[e.CounterID]; !ok {
			v.due[e.CounterID] = time.Time{}
		}
	}
	return nil
}

func (v *totals) onReset(_ context.Context, e *CounterReset) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.totals[e.CounterID] = 0
	delete(v.due, e.CounterID)
	return nil
}

func (v *totals) Total(id string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.totals[id]
}

// Claim makes totals an automation.TodoList.
func (v *totals) Claim(_ context.Context, req automation.ClaimRequest) ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	var ids []string
	for id, until := range v.due {
		if until.After(now) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > req.MaxPerKey {
		ids = ids[:req.MaxPerKey]
	}
	for _, id := range ids {
		v.due[id] = now.Add(req.Lease)
	}
	return ids, nil
}

func resetAutomation(list *totals, todo automation.Dependency, bus eventsourcing.Dispatcher) automation.Definition[string] {
	return automation.Definition[string]{
		Name:       "reset-counters",
		TodoList:   list,
		Projection: todo,
		Work: func(ctx context.Context, id string) error {
			_, err := bus.Dispatch(ctx, ResetCounter{CounterID: id})
			return err
		},
	}
}

// --- command slice ---

func TestCommand_RecordsEvents(t *testing.T) {
	slicetest.Command(t, incrementHandler, slicetest.IgnoreFields("At")).
		Given(&Incremented{CounterID: "c1", By: 2}).
		When(Increment{CounterID: "c1", By: 3}).
		Then(&Incremented{CounterID: "c1", By: 3})
}

func TestCommand_Rejected(t *testing.T) {
	slicetest.Command(t, incrementHandler).
		When(Increment{CounterID: "c1", By: 0}).
		ThenRejectedWith(errNonPositive)

	slicetest.Command(t, resetHandler).
		When(ResetCounter{CounterID: "fresh"}).
		ThenRejected()
}

func TestCommand_FoldsGivenHistory(t *testing.T) {
	// The handler sees the given events: a counter with history can be reset.
	slicetest.Command(t, resetHandler).
		Given(&Incremented{CounterID: "c1", By: 4}, &Incremented{CounterID: "c1", By: 1}).
		When(ResetCounter{CounterID: "c1"}).
		Then(&CounterReset{CounterID: "c1"})
}

func TestCommand_DiffReportsTheDifference(t *testing.T) {
	ft := &fakeT{TB: t}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(stop); !ok {
					panic(r)
				}
			}
		}()
		slicetest.Command(ft, incrementHandler, slicetest.IgnoreFields("At")).
			When(Increment{CounterID: "c1", By: 3}).
			Then(&Incremented{CounterID: "c1", By: 4})
	}()
	if !ft.failed || !strings.Contains(ft.msg, "got:") || !strings.Contains(ft.msg, "By:3") {
		t.Fatalf("expected a diff mentioning the By field, got failed=%v msg=%q", ft.failed, ft.msg)
	}
}

func TestCommand_EnvelopesCarryVersions(t *testing.T) {
	out := slicetest.Command(t, incrementHandler).
		Given(&Incremented{CounterID: "c1", By: 1}).
		When(Increment{CounterID: "c1", By: 1})
	envs := out.Envelopes()
	if len(envs) != 1 || envs[0].Version != 2 || envs[0].StreamID != "c1" {
		t.Fatalf("envelopes = %+v, want one at version 2 on stream c1", envs)
	}
}

// --- view slice ---

func TestView_ReadModelAfterEvents(t *testing.T) {
	view := newTotals(10)
	slicetest.View(t, view.projection()).
		Given(&Incremented{CounterID: "c1", By: 3}, &Incremented{CounterID: "c1", By: 4}, &Incremented{CounterID: "c2", By: 1}).
		Then(func(t testing.TB) {
			if got := view.Total("c1"); got != 7 {
				t.Fatalf("c1 total = %d, want 7", got)
			}
			if got := view.Total("c2"); got != 1 {
				t.Fatalf("c2 total = %d, want 1", got)
			}
		})
}

// --- scenario ---

func TestScenario_AutomationResetsCounterAtLimit(t *testing.T) {
	s := slicetest.New(t)
	view := newTotals(10)
	todo := s.Projection(view.projection())
	slicetest.Handle(s, incrementHandler(s.Store()))
	slicetest.Handle(s, resetHandler(s.Store()))
	slicetest.Automation(s, resetAutomation(view, todo, s.Bus()))

	s.Given(&Incremented{CounterID: "c1", By: 6}, &Incremented{CounterID: "c1", By: 5})
	s.Run()
	s.Then(&CounterReset{CounterID: "c1"})
	if got := view.Total("c1"); got != 0 {
		t.Fatalf("read model total = %d after the reset, want 0", got)
	}
}

func TestScenario_NothingHappensBelowTheLimit(t *testing.T) {
	s := slicetest.New(t)
	view := newTotals(10)
	todo := s.Projection(view.projection())
	slicetest.Handle(s, resetHandler(s.Store()))
	slicetest.Automation(s, resetAutomation(view, todo, s.Bus()))

	s.Given(&Incremented{CounterID: "c1", By: 4})
	s.Run()
	s.ThenNothing()
}

func TestScenario_DispatchThroughTheBus(t *testing.T) {
	s := slicetest.New(t, slicetest.IgnoreFields("At"))
	slicetest.Handle(s, incrementHandler(s.Store()))

	if err := s.Dispatch(Increment{CounterID: "c1", By: 2}); err != nil {
		t.Fatal(err)
	}
	s.Then(&Incremented{CounterID: "c1", By: 2})
}

// --- recording dispatcher ---

func TestRecordingDispatcher(t *testing.T) {
	bus := slicetest.NewRecordingDispatcher()
	bus.Reject("ResetCounter", eventsourcing.NewBusinessRuleViolation(errors.New("already reset")))

	if _, err := bus.Dispatch(context.Background(), Increment{CounterID: "c1", By: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := bus.Dispatch(context.Background(), ResetCounter{CounterID: "c1"})
	var rejected *eventsourcing.BusinessRuleViolationError
	if !errors.As(err, &rejected) {
		t.Fatalf("err = %v, want a business rule violation", err)
	}
	got := bus.Dispatched()
	if len(got) != 2 || got[0].CommandType() != "Increment" || got[1].CommandType() != "ResetCounter" {
		t.Fatalf("dispatched %v", got)
	}
}

// fakeT captures a fatal failure instead of ending the test.
type fakeT struct {
	testing.TB
	failed bool
	msg    string
}

func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
	panic(stop{})
}

func (f *fakeT) Helper() {}

type stop struct{}
