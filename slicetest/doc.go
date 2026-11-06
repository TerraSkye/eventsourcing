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
//	slicetest.Command(t, createtask.Handler).
//		Given(&events.TaskCreated{TaskID: id, Title: "Write docs"}).
//		When(createtask.CreateTask{TaskID: id, Title: "Write docs"}).
//		ThenRejected()
//
// The handler folds the state itself, so the test touches nothing private.
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
//	s := slicetest.New(t)
//	todo := s.Projection(taskstoarchive.NewProjection(list))
//	slicetest.Handle(s, archivetask.Handler(s.Store()))
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
// Then compares events by type and exported fields. Fields a handler
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
