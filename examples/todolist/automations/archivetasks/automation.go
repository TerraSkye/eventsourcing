// Package archivetasks is the automation of Part 5: it archives the tasks
// the tasks-to-archive todo list says are due.
package archivetasks

import (
	"context"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
)

func NewAutomation(
	list *taskstoarchive.TasksToArchive,
	todoList automation.Dependency,
	archive eventsourcing.CommandHandler[archivetask.ArchiveTask],
) automation.Definition[uuid.UUID] {
	return automation.Definition[uuid.UUID]{
		Name:     "archive-tasks",
		TodoList: todoList,
		Claim:    list.Claim,
		Work: func(ctx context.Context, taskID uuid.UUID) error {
			_, err := archive(ctx, archivetask.ArchiveTask{TaskID: taskID})
			return err
		},
	}
}
