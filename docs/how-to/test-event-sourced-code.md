# How to test event-sourced code

The `slicetest` package gives each kind of slice one test shape, over the
public API and the memory stores: `slicetest.Command` for a command
handler (given events, when a command, then events or a rejection),
`slicetest.View` for a projection (given events, then the read model), and
`slicetest.New` for a scenario that crosses slices (given events, run the
automations, then the events they caused). The sections below show the
same tests written by hand, which is still right when a test needs
something the fixtures do not cover.

```go
func TestCreateTask_RejectsDuplicate(t *testing.T) {
    id := uuid.New()
    slicetest.Command(t, createtask.NewHandler).
        Given(&events.TaskCreated{TaskID: id, Title: "Write docs"}).
        When(createtask.CreateTask{TaskID: id, Title: "Write docs"}).
        ThenRejected()
}

func TestArchiveTasks_ArchivesAfterSevenDays(t *testing.T) {
    s := slicetest.New(t)
    list := taskstoarchive.NewTasksToArchive(7 * 24 * time.Hour)
    todo := s.Projection(taskstoarchive.NewProjection(list))
    slicetest.Handle(s, archivetask.NewHandler(s.Store()))
    slicetest.Automation(s, archivetasks.New(list, todo, s.Bus()).Definition())

    id := uuid.New()
    s.Given(&events.TaskCompleted{TaskID: id, CompletedAt: time.Now().Add(-8 * 24 * time.Hour)})
    s.Run()
    s.Then(&events.TaskArchived{TaskID: id})
}
```

Test commands, queries, and projectors using the in-memory stores.

## Testing the decide function directly

`decide` is a pure function — test it without any infrastructure:

```go
package createtask_test

import (
    "testing"
    "github.com/google/uuid"
    // import your package
)

func TestDecide_NewTask(t *testing.T) {
    state := taskState{Exists: false}
    cmd := CreateTask{TaskID: uuid.New(), Title: "Test task"}

    evts, err := decide(state, cmd)

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if len(evts) != 1 {
        t.Fatalf("expected 1 event, got %d", len(evts))
    }
    created, ok := evts[0].(*events.TaskCreated)
    if !ok {
        t.Fatal("expected TaskCreated")
    }
    if created.Title != "Test task" {
        t.Errorf("expected title 'Test task', got %q", created.Title)
    }
}

func TestDecide_DuplicateTask(t *testing.T) {
    state := taskState{Exists: true}
    cmd := CreateTask{TaskID: uuid.New(), Title: "Duplicate"}

    _, err := decide(state, cmd)

    if err == nil {
        t.Fatal("expected error for duplicate task")
    }
}
```

## Testing a command handler end-to-end

Use the in-memory store as a real store:

```go
package createtask_test

import (
    "context"
    "testing"

    "github.com/google/uuid"
    "github.com/terraskye/eventsourcing/memory"
    "your-module/slices/createtask"
)

func TestCreateTask_Success(t *testing.T) {
    store := memory.NewEventStore()
    defer store.Close()

    handler := createtask.NewHandler(store)

    cmd := createtask.CreateTask{
        TaskID: uuid.New(),
        Title:  "Test task",
    }

    result, err := handler(context.Background(), cmd)
    if err != nil {
        t.Fatal(err)
    }
    if !result.Successful {
        t.Error("expected successful result")
    }
    if result.NextExpectedVersion != 1 {
        t.Errorf("expected version 1, got %d", result.NextExpectedVersion)
    }
}

func TestCreateTask_DuplicateID(t *testing.T) {
    store := memory.NewEventStore()
    defer store.Close()

    handler := createtask.NewHandler(store)
    cmd := createtask.CreateTask{TaskID: uuid.New(), Title: "Task"}

    // First call succeeds
    if _, err := handler(context.Background(), cmd); err != nil {
        t.Fatal(err)
    }

    // Second call with the same ID fails
    _, err := handler(context.Background(), cmd)
    if err == nil {
        t.Fatal("expected error on duplicate")
    }
}
```

## Testing business rule violations

Check that the error is a `BusinessRuleViolationError`:

```go
import (
    "errors"
    "github.com/terraskye/eventsourcing"
)

func TestCompleteTask_NotExist(t *testing.T) {
    store := memory.NewEventStore()
    defer store.Close()

    handler := completetask.NewHandler(store)
    cmd := completetask.CompleteTask{TaskID: uuid.New()}

    _, err := handler(context.Background(), cmd)

    var violation *eventsourcing.BusinessRuleViolationError
    if !errors.As(err, &violation) {
        t.Fatalf("expected BusinessRuleViolationError, got %T: %v", err, err)
    }
}
```

## Testing a query handler

```go
func TestListTasks_Empty(t *testing.T) {
    store := memory.NewEventStore()
    defer store.Close()

    qh := listtasks.NewQueryHandler(store)
    result, err := qh.HandleQuery(context.Background(), listtasks.ListTasks{})

    if err != nil {
        t.Fatal(err)
    }
    if len(result.Tasks) != 0 {
        t.Errorf("expected 0 tasks, got %d", len(result.Tasks))
    }
}

func TestListTasks_AfterCreation(t *testing.T) {
    store := memory.NewEventStore()
    defer store.Close()

    // Create a task via command handler (exercises real store writes)
    createHandler := createtask.NewHandler(store)
    _, _ = createHandler(context.Background(), createtask.CreateTask{
        TaskID: uuid.New(),
        Title:  "My task",
    })

    qh := listtasks.NewQueryHandler(store)
    result, _ := qh.HandleQuery(context.Background(), listtasks.ListTasks{})

    if len(result.Tasks) != 1 {
        t.Fatalf("expected 1 task, got %d", len(result.Tasks))
    }
    if result.Tasks[0].Title != "My task" {
        t.Errorf("unexpected title: %q", result.Tasks[0].Title)
    }
}
```

## Testing a projector

```go
import (
    "context"
    "github.com/terraskye/eventsourcing/memory"
)

func TestProjector_OnTaskCreated(t *testing.T) {
    projector := tasklist.NewProjector()

    _ = projector.OnTaskCreated(context.Background(), &events.TaskCreated{
        TaskID: uuid.New(),
        Title:  "Projected task",
    })

    tasks := projector.All()
    if len(tasks) != 1 {
        t.Fatalf("expected 1 task, got %d", len(tasks))
    }
    if tasks[0].Title != "Projected task" {
        t.Errorf("unexpected title: %q", tasks[0].Title)
    }
}
```

## Tips

- **Test `decide` and `evolve` in isolation** — they are pure functions and require no infrastructure.
- **Use `memory.NewEventStore`** for integration tests that need a real store.
- **Use `memory.NewProjectionStore`** with a `projection.Runner` for tests of a projection.
- **Close stores** with `defer store.Close()` to avoid goroutine leaks in tests.
- **Register every event type** — `memory.NewEventStore` encodes events and checks the registry as the persistent stores do, so `Save` rejects an unregistered type. Registering in an `init` beside each event, as the tutorials do, covers every test that imports the package. The registry is global, and registering a name twice panics, so do not register again in a test.
