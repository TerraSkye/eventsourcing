package slicetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
)

// Scenario wires the pieces a slice crosses: a memory event store, a
// projection store, an automation store and a command bus. Projections
// run in the background; automations run to completion on [Scenario.Run];
// command handlers answer the bus. Build one with [NewScenario].
//
//	s := slicetest.NewScenario(t, slicetest.IgnoreFields("ArchivedAt"))
//	todo := s.Projection(taskstoarchive.NewProjection(list))
//	slicetest.Handle(s, archivetask.NewHandler(s.Store()))
//	slicetest.Automation(s, archivetasks.New(list, todo, s.Bus()).Definition())
//
//	s.Given(&events.TaskCompleted{TaskID: id, CompletedAt: eightDaysAgo})
//	s.Run()
//	s.Then(&events.TaskArchived{TaskID: id})
type Scenario struct {
	t           testing.TB
	log         *log
	bus         *eventsourcing.CommandBus
	autoStore   *memory.AutomationStore
	projections []*projection.Runner
	automations []scenarioAutomation
	since       uint64

	// MaxRounds bounds Scenario.Run: each round waits for the projections
	// and runs every automation once, and Run stops when a round changed
	// nothing. A scenario still changing after MaxRounds fails the test.
	// Default 10.
	MaxRounds int
}

type scenarioAutomation struct {
	name    string
	runOnce func(ctx context.Context) (automation.Summary, error)
}

// NewScenario builds an empty scenario. Options: [IgnoreFields],
// [StreamID].
//
// Every fixture is built on the test it is given, so a table test builds
// one scenario per row, inside t.Run, and its rows may run in parallel.
func NewScenario(t testing.TB, opts ...Option) *Scenario {
	t.Helper()
	bus := eventsourcing.NewCommandBus(64, 4)
	t.Cleanup(bus.Stop)
	return &Scenario{
		t:         t,
		log:       newLog(t, newOptions(opts)),
		bus:       bus,
		autoStore: memory.NewAutomationStore(),
		MaxRounds: 10,
	}
}

// Store is the memory event store every piece reads and writes.
func (s *Scenario) Store() eventsourcing.EventStore { return s.log.store }

// Bus is the command bus; register handlers with [Handle] and pass it to
// anything that dispatches.
func (s *Scenario) Bus() *eventsourcing.CommandBus { return s.bus }

// AutomationStore is the memory store automations lease segments in.
func (s *Scenario) AutomationStore() automation.Store { return s.autoStore }

// Handle registers a command handler on the scenario's bus.
func Handle[C eventsourcing.Command](s *Scenario, handler eventsourcing.CommandHandler[C]) {
	eventsourcing.Register(s.bus, handler)
}

// Projection starts def on the scenario's stores and returns its runner,
// which is what an automation's Definition.Projection takes. It polls
// every 10ms unless def sets a PollInterval. Given and Run wait for every
// projection to catch up.
func (s *Scenario) Projection(def projection.Definition) *projection.Runner {
	s.t.Helper()
	r := startProjection(s.t, def, s.log.store)
	s.projections = append(s.projections, r)
	return r
}

// Automation adds def to the scenario. Its Store defaults to the
// scenario's automation store and its PollInterval to 10ms; its
// Projection must be a runner from [Scenario.Projection]. The automation
// does not run in the background: [Scenario.Run] runs it to completion.
func Automation[T any](s *Scenario, def automation.Definition[T]) {
	s.t.Helper()
	if def.Store == nil && def.Scheduler == nil {
		def.Store = s.autoStore
	}
	if def.PollInterval == 0 {
		def.PollInterval = 10 * time.Millisecond
	}
	runner := automation.NewRunner(def)
	s.automations = append(s.automations, scenarioAutomation{name: def.Name, runOnce: runner.RunOnce})
}

// Given records events as history and waits for every projection to
// apply them. Then compares against what is recorded after this point.
func (s *Scenario) Given(events ...eventsourcing.Event) *Scenario {
	s.t.Helper()
	s.since = s.log.seed(events)
	s.catchUp()
	return s
}

// catchUp waits until every projection has applied the whole log.
func (s *Scenario) catchUp() {
	s.t.Helper()
	head := s.log.head()
	for _, r := range s.projections {
		waitLive(s.t, r, head)
	}
}

// Run lets the model react: projections catch up, every automation runs
// once, their commands are handled, and the round repeats until a round
// changes nothing. It returns the summaries of the last round, one per
// automation in the order they were added.
//
// It fails the test when the scenario still changes after MaxRounds, or
// when one automation's run does not finish within 10 seconds, which
// happens when its todo list keeps handing out the same work.
func (s *Scenario) Run() []automation.Summary {
	s.t.Helper()
	var last []automation.Summary
	for range s.MaxRounds {
		s.catchUp()
		before := s.log.head()
		last = last[:0]
		moved := false
		for _, a := range s.automations {
			ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
			sum, err := a.runOnce(ctx)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				s.t.Fatalf("slicetest: automation %q did not finish its run within 10s; "+
					"does its todo list drop items once they are worked?", a.name)
			}
			if err != nil {
				s.t.Fatalf("slicetest: automation %q: %v", a.name, err)
			}
			last = append(last, sum)
			if sum.Processed > 0 || sum.Failed > 0 || sum.Deferred > 0 {
				moved = true
			}
		}
		s.catchUp()
		if !moved && s.log.head() == before {
			return last
		}
	}
	s.t.Fatalf("slicetest: scenario still changing after %d rounds; raise MaxRounds "+
		"if that is expected, or look for automations that keep triggering each other", s.MaxRounds)
	return last
}

// Then asserts that exactly these events were recorded since Given, in
// this order, across all streams.
func (s *Scenario) Then(events ...eventsourcing.Event) {
	s.t.Helper()
	got := s.log.since(s.since)
	if diff := s.log.opts.compare(events, got); diff != "" {
		s.t.Fatalf("recorded events differ:\n%s  got %s, want %s", diff, typeNames(got), typeNames(events))
	}
}

// ThenNothing asserts that nothing was recorded since Given.
func (s *Scenario) ThenNothing() {
	s.t.Helper()
	s.Then()
}

// Events returns what was recorded since Given, for custom assertions.
func (s *Scenario) Events() []eventsourcing.Event {
	s.t.Helper()
	return s.log.since(s.since)
}

// Envelopes returns what was recorded since Given with envelopes, for
// assertions on correlation and causation across the slice.
func (s *Scenario) Envelopes() []*eventsourcing.Envelope {
	s.t.Helper()
	return s.log.envelopesSince(s.since)
}

// Dispatch sends a command through the bus, as an HTTP handler would, and
// waits for the projections to apply its events. It returns the handler's
// error.
func (s *Scenario) Dispatch(cmd eventsourcing.Command) error {
	s.t.Helper()
	_, err := s.bus.Dispatch(s.t.Context(), cmd)
	s.catchUp()
	return err
}
