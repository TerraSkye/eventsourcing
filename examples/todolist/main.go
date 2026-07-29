// Command todolist is the worked example from the terraskye/eventsourcing
// tutorials (see docs/tutorials/): a small task management API that
// demonstrates commands, queries, real-time projections via the event bus,
// and background processing with a saga-style processor.
package main

import (
	"context"
	"log"
	"time"

	"github.com/gin-gonic/gin"

	membus "github.com/terraskye/eventsourcing/eventbus/memory"
	memstore "github.com/terraskye/eventsourcing/eventstore/memory"

	"github.com/terraskye/eventsourcing/examples/todolist/processors/archivetasks"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/completetask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/createtask"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/tasklist"
)

func main() {
	store := memstore.NewMemoryStore(100)
	defer store.Close()

	bus := membus.NewEventBus(100)
	defer bus.Close()

	// Cached projection, kept up to date in real time (Part 4).
	projector := tasklist.NewProjector()
	if err := bus.Subscribe(context.Background(), "task-list-projector", projector.EventHandlers()); err != nil {
		log.Fatal(err)
	}

	// Auto-archive completed tasks after a delay (Part 5).
	// 5 seconds here for a quick demo; production would use 30*24*time.Hour.
	archiveHandler := archivetask.NewHandler(store)
	archiveProcessor := archivetasks.NewProcessor(archiveHandler, 5*time.Second)
	if err := bus.Subscribe(context.Background(), "archive-processor", archiveProcessor.EventHandlers()); err != nil {
		log.Fatal(err)
	}

	// Forward events from the store to the bus.
	go func() {
		for env := range store.Events() {
			bus.Dispatch(env)
		}
	}()

	createTaskHTTP := createtask.NewHTTPHandler(createtask.NewHandler(store))
	completeTaskHTTP := completetask.NewHTTPHandler(completetask.NewHandler(store))
	listTasksHTTP := tasklist.NewHTTPHandler(tasklist.NewQueryHandler(projector))

	r := gin.Default()
	tasks := r.Group("/api/v1/tasks")
	createTaskHTTP.RegisterRoutes(tasks)
	completeTaskHTTP.RegisterRoutes(tasks)
	listTasksHTTP.RegisterRoutes(tasks)

	log.Fatal(r.Run(":8080"))
}
