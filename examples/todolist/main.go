// Command todolist is the worked example from the terraskye/eventsourcing
// tutorials (see docs/tutorials/): a small task management API that
// demonstrates commands, queries, a projection kept up to date by the
// projection runner, and an automation that works off a todo list.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/logging"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"

	"github.com/terraskye/eventsourcing/examples/todolist/automations/archivetasks"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/completetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/createtask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/tasklist"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
)

func main() {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	commandBus := eventsourcing.NewCommandBus(20, 1)
	commandBus.Use(logging.CommandLogging(logger))
	defer commandBus.Stop()

	store := memory.NewEventStore(100)
	defer store.Close()

	eventsourcing.Register(commandBus, completetask.NewHandler(store))
	eventsourcing.Register(commandBus, createtask.NewHandler(store))

	// The task list, kept up to date by the projection runner (Part 4).
	list := tasklist.NewTaskList()
	p := tasklist.NewProjection(list)
	p.Logger = logger
	taskList := projection.NewRunner(p, store, memory.NewProjectionStore())

	// Archive completed tasks once they are due (Part 5): a todo list and
	// the automation that works off it. 5 seconds here for a quick demo;
	// production would use 30*24*time.Hour.
	archiveList := taskstoarchive.NewTasksToArchive(5 * time.Second)
	todo := taskstoarchive.NewProjection(archiveList)
	todo.Logger = logger
	archiveTodo := projection.NewRunner(todo, store, memory.NewProjectionStore())

	archive := archivetasks.NewAutomation(archiveList, archiveTodo, archivetask.NewHandler(store))
	archive.Logger = logger
	archive.Interval = time.Second
	archiveTasks := automation.New(archive)

	for _, run := range []func(context.Context) error{taskList.Run, archiveTodo.Run, archiveTasks.Run} {
		go func() {
			if err := run(ctx); err != nil && ctx.Err() == nil {
				log.Fatal(err)
			}
		}()
	}

	createTaskHTTP := createtask.NewHTTPHandler(commandBus)
	completeTaskHTTP := completetask.NewHTTPHandler(commandBus)
	listTasksHTTP := tasklist.NewHTTPHandler(tasklist.NewQueryHandler(list))

	r := gin.Default()
	tasks := r.Group("/api/v1/tasks")
	createTaskHTTP.RegisterRoutes(tasks)
	completeTaskHTTP.RegisterRoutes(tasks)
	listTasksHTTP.RegisterRoutes(tasks)

	log.Fatal(r.Run(":9000"))
}
