// Package slicetest tests event-sourced applications one slice at a time,
// the way Event Modeling draws them: a command slice turns events and a
// command into events, a view slice turns events into a read model, an
// automation slice turns events into commands. Each has one test shape,
// and every fixture runs on the memory stores, so a slice test needs no
// database and finishes in milliseconds.
//
// # Command slices
//
// [Command] builds a handler on a memory event store, seeds it with past
// events, sends one command, and compares the events it recorded:
//
//	slicetest.Command(t, createtask.NewHandler).
//		Given(&events.TaskCreated{TaskID: id, Title: "Write docs"}).
//		When(createtask.CreateTask{TaskID: id, Title: "Write docs"}).
//		ThenRejected()
//
// The handler folds the state itself, so the test touches nothing private.
//
// # Table tests
//
// Most slice tests are tables: one row per case, each with its own
// history, command and expected outcome. [Outcome.Expect] takes the
// expected error and events of a row in one call, so the loop body is the
// same for a success, a rejection and an infrastructure error:
//
//	tests := []struct {
//		name    string
//		given   []eventsourcing.Event
//		when    createtask.CreateTask
//		wantErr error
//		want    []eventsourcing.Event
//	}{
//		{name: "creates", when: create, want: []eventsourcing.Event{&events.TaskCreated{TaskID: id, Title: "Write docs"}}},
//		{name: "duplicate", given: []eventsourcing.Event{&events.TaskCreated{TaskID: id}}, when: create, wantErr: slicetest.ErrRejected},
//	}
//	for _, tt := range tests {
//		t.Run(tt.name, func(t *testing.T) {
//			t.Parallel()
//			slicetest.Command(t, createtask.NewHandler).
//				Given(tt.given...).
//				When(tt.when).
//				Expect(tt.wantErr, tt.want...)
//		})
//	}
//
// Every fixture is built on the t it is given, with stores of its own, so
// rows share nothing and may run in parallel. Given, Then and Expect take
// a nil slice as "nothing", so empty columns need no special case.
//
// # View slices
//
// [View] runs a projection on memory stores, feeds it events as real
// envelopes (version, position, metadata) through a projection.Runner,
// waits until it is live, and hands back to the test for assertions on
// the read model:
//
//	slicetest.View(t, tasklist.NewProjection(list)).
//		Given(&events.TaskCreated{TaskID: id, Title: "Write docs"}).
//		Then(func(t testing.TB) {
//			if len(list.All()) != 1 { t.Fatal("expected one task") }
//		})
//
// # Automation slices and whole scenarios
//
// [Scenario] wires a memory event store, projection store, automation
// store and command bus together, so a test can follow a slice across
// the model: events in, the todo-list projection catches up, the
// automation runs once, its commands are handled, and the resulting
// events are compared.
//
//	s := slicetest.NewScenario(t, slicetest.IgnoreFields("ArchivedAt"))
//	todo := s.Projection(taskstoarchive.NewProjection(list))
//	slicetest.Handle(s, archivetask.NewHandler(s.Store()))
//	slicetest.Automation(s, archivetasks.New(list, todo, s.Bus()).Definition())
//
//	s.Given(&events.TaskCompleted{TaskID: id, CompletedAt: eightDaysAgo})
//	s.Run()
//	s.Then(&events.TaskArchived{TaskID: id})
//
// [RecordingDispatcher] stands in for the command bus when a Work
// function or an HTTP handler is tested on its own.
//
// # Comparing events
//
// Then compares events by type and exported fields; *T and T compare as
// the same type. Fields a handler
// fills in at runtime, a timestamp or a generated id, are excluded with
// [IgnoreFields]; anything else is a custom check on [Outcome.Events].
//
// # What this package is not
//
// It does not verify a Claim query against a database (that is
// automationtest.ClaimAcceptanceTest), nor a store backend (the
// acceptance suites in eventsourcingtest, projectiontest and
// automationtest). It tests the slice's own logic, over the public API.
package slicetest
