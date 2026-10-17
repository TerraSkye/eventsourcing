// Command todolist is the worked example from the terraskye/eventsourcing
// tutorials (see docs/tutorials/): a small task management API that
// demonstrates commands, queries, real-time projections via the event bus,
// and background processing with a saga-style processor.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/processors/archivetasks"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/logging"

	membus "github.com/terraskye/eventsourcing/eventbus/memory"
	memstore "github.com/terraskye/eventsourcing/eventstore/memory"

	"github.com/terraskye/eventsourcing/examples/todolist/slices/completetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/createtask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/tasklist"
)

func main() {

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	commandBus := eventsourcing.NewCommandBus(20, 1)
	commandBus.Use(logging.CommandLogging(logger))

	defer commandBus.Stop()

	store := memstore.NewMemoryStore(100)
	defer store.Close()

	eventBus := membus.NewEventBus(100)
	eventBus.Use(logging.EventLogging(logger))
	defer eventBus.Close()

	eventsourcing.Register(commandBus, archivetask.NewHandler(store))
	eventsourcing.Register(commandBus, completetask.NewHandler(store))
	eventsourcing.Register(commandBus, createtask.NewHandler(store))

	// Cached projection, kept up to date in real time (Part 4).
	projector := tasklist.NewProjector()

	if err := eventBus.Subscribe(context.Background(), "task-list-projector", projector.EventHandlers(), membus.WithFilterEvents(projector.EventHandlers().StreamFilter())); err != nil {
		log.Fatal(err)
	}

	// Auto-archive completed tasks after a delay (Part 5).
	// 5 seconds here for a quick demo; production would use 30*24*time.Hour.
	archiveProcessor := archivetasks.NewProcessor(commandBus, 5*time.Second)
	if err := eventBus.Subscribe(context.Background(), "archive-processor", archiveProcessor.EventHandlers(), membus.WithFilterEvents(archiveProcessor.EventHandlers().StreamFilter())); err != nil {
		log.Fatal(err)
	}

	// Forward events from the store to the bus.
	go func() {
		for env := range store.Events() {
			eventBus.Dispatch(env)
		}
	}()

	createTaskHTTP := createtask.NewHTTPHandler(commandBus)
	completeTaskHTTP := completetask.NewHTTPHandler(commandBus)
	listTasksHTTP := tasklist.NewHTTPHandler(tasklist.NewQueryHandler(projector))

	r := gin.Default()
	tasks := r.Group("/api/v1/tasks")
	createTaskHTTP.RegisterRoutes(tasks)
	completeTaskHTTP.RegisterRoutes(tasks)
	listTasksHTTP.RegisterRoutes(tasks)

	log.Fatal(r.Run(":9000"))
}
