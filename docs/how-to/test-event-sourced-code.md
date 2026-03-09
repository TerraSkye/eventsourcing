# How to test event-sourced code

Test commands, queries, and projectors using the in-memory store and event bus.

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
    memstore "github.com/terraskye/eventsourcing/eventstore/memory"
    "your-module/slices/createtask"
)

func TestCreateTask_Success(t *testing.T) {
    store := memstore.NewMemoryStore(10)
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
    store := memstore.NewMemoryStore(10)
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

Check that the error is a `ErrBusinessRuleViolation`:

```go
import (
    "errors"
    "github.com/terraskye/eventsourcing"
)

func TestCompleteTask_NotExist(t *testing.T) {
    store := memstore.NewMemoryStore(10)
    defer store.Close()

    handler := completetask.NewHandler(store)
    cmd := completetask.CompleteTask{TaskID: uuid.New()}

    _, err := handler(context.Background(), cmd)

    var violation *eventsourcing.ErrBusinessRuleViolation
    if !errors.As(err, &violation) {
        t.Fatalf("expected ErrBusinessRuleViolation, got %T: %v", err, err)
    }
}
```

## Testing a query handler

```go
func TestListTasks_Empty(t *testing.T) {
    store := memstore.NewMemoryStore(10)
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
    store := memstore.NewMemoryStore(10)
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
    membus "github.com/terraskye/eventsourcing/eventbus/memory"
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
- **Use `memstore.NewMemoryStore`** for integration tests that need a real store.
- **Use `membus.NewEventBus`** for tests that involve event bus subscriptions.
- **Close stores and buses** with `defer store.Close()` / `defer bus.Close()` to avoid goroutine leaks in tests.
- **Avoid global state** — the event registry uses package-level globals; register events in `TestMain` if needed.
