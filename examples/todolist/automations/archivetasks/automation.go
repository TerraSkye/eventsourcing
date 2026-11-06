// Package archivetasks is the automation of Part 5: it archives the tasks
// the tasks-to-archive todo list says are due.
//
// The automation takes nothing off the list itself. It dispatches the
// ArchiveTask command; the TaskArchived event that follows makes the
// projection remove the task. Until then, the item's lease keeps it from
// being claimed a second time.
package archivetasks

import (
	"context"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
)

// ArchiveTasks is the automation as a type: the todo list it claims from,
// the projection that keeps that list, and the dispatcher its commands go
// to. Work is a method, so the automation can be tested without a runner.
type ArchiveTasks struct {
	list *taskstoarchive.TasksToArchive
	todo automation.Dependency
	bus  eventsourcing.Dispatcher
}

// New returns the automation. todo is the runner (or probe) of the
// projection that keeps list; bus is the command bus, with the archivetask
// handler registered on it.
func New(
	list *taskstoarchive.TasksToArchive,
	todo automation.Dependency,
	bus eventsourcing.Dispatcher,
) *ArchiveTasks {
	return &ArchiveTasks{list: list, todo: todo, bus: bus}
}

// Definition describes the automation for automation.NewRunner: what it
// works and when. Where its leases live is main's choice, as it is for the
// projection store, so main sets Definition.Store:
//
//	archive := archivetasks.New(list, todo, commandBus).Definition()
//	archive.Store = memory.NewAutomationStore()
//	go automation.NewRunner(archive).Run(ctx)
func (a *ArchiveTasks) Definition() automation.Definition[uuid.UUID] {
	return automation.Definition[uuid.UUID]{
		Name:       "archive-tasks",
		TodoList:   a.list,
		Projection: a.todo, // work only while the list is live
		Work:       a.Work,
	}
}

// Work archives one due task. The command goes through the bus, so the
// bus's middleware applies to it as to any other command. A task that was
// archived meanwhile is not an error: the decider appends nothing.
func (a *ArchiveTasks) Work(ctx context.Context, taskID uuid.UUID) error {
	_, err := a.bus.Dispatch(ctx, archivetask.ArchiveTask{TaskID: taskID})
	return err
}
