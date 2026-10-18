// Command todolist is the worked example from the terraskye/eventsourcing
// tutorials (see docs/tutorials/): a small task management API that
// demonstrates commands, queries, a projection kept up to date by the
// projection runner, and an automation that works off a todo list.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Completed tasks are archived after 5 seconds for a quick demo;
	// production would use 30*24*time.Hour.
	a := newApp(logger, 5*time.Second, time.Second)
	defer a.close()

	srv := &http.Server{Addr: ":9000", Handler: a.routes}
	go func() {
		if err := a.run(ctx); err != nil {
			logger.Error("background process failed", slog.String("error", err.Error()))
		}
		stop() // a failed background process takes the server down with it
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx) // the server is stopping either way
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// app is the wired service: the HTTP routes and the projection runners and
// automation behind them.
type app struct {
	routes     http.Handler
	background []func(context.Context) error
	close      func()
}

// newApp wires the store, the command bus, the slices and the HTTP routes.
// Completed tasks are archived archiveAfter their completion; the
// automation checks for due tasks every checkEvery.
func newApp(logger *slog.Logger, archiveAfter, checkEvery time.Duration) *app {
	commandBus := eventsourcing.NewCommandBus(20, 1)
	commandBus.Use(logging.CommandLogging(logger))

	store := memory.NewEventStore(100)

	eventsourcing.Register(commandBus, completetask.NewHandler(store))
	eventsourcing.Register(commandBus, createtask.NewHandler(store))

	// The task list, kept up to date by the projection runner (Part 4).
	list := tasklist.NewTaskList()
	p := tasklist.NewProjection(list)
	p.Logger = logger
	taskList := projection.NewRunner(p, store, memory.NewProjectionStore())

	// Archive completed tasks once they are due (Part 5): a todo list and
	// the automation that works off it.
	archiveList := taskstoarchive.NewTasksToArchive(archiveAfter)
	todo := taskstoarchive.NewProjection(archiveList)
	todo.Logger = logger
	archiveTodo := projection.NewRunner(todo, store, memory.NewProjectionStore())

	archive := archivetasks.NewAutomation(archiveList, archiveTodo, archivetask.NewHandler(store))
	archive.Logger = logger
	archive.Interval = checkEvery
	archiveTasks := automation.New(archive)

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	tasks := r.Group("/api/v1/tasks")
	createtask.NewHTTPHandler(commandBus, taskList).RegisterRoutes(tasks)
	completetask.NewHTTPHandler(commandBus, taskList).RegisterRoutes(tasks)
	tasklist.NewHTTPHandler(tasklist.NewQueryHandler(list)).RegisterRoutes(tasks)

	return &app{
		routes:     r,
		background: []func(context.Context) error{taskList.Run, archiveTodo.Run, archiveTasks.Run},
		close: func() {
			commandBus.Stop()
			store.Close()
		},
	}
}

// run runs the projection runners and the automation until ctx is done or
// one of them fails, and returns the first failure.
func (a *app) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errc := make(chan error, len(a.background))
	for _, run := range a.background {
		go func() { errc <- run(ctx) }()
	}

	var first error
	for range a.background {
		if err := <-errc; err != nil && ctx.Err() == nil && first == nil {
			first = err
			cancel() // stop the others
		}
	}
	return first
}
