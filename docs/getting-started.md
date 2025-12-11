# Tutorial: Event Sourcing for Go Developers

Welcome! This tutorial teaches you event sourcing by building a **Task Management API** from scratch using terraskye/eventsourcing and Gin.

## What You'll Build

A production-ready task management system with:

- ✅ **Commands**: Create tasks, assign them, mark complete
- ✅ **Queries**: View task lists, personal dashboards
- ✅ **Real-time updates**: Projections that update automatically
- ✅ **Background work**: Auto-archive completed tasks
- ✅ **Multiple views**: Different interfaces for team members vs managers

## Prerequisites

- **Go 1.21+** installed
- Basic understanding of HTTP APIs
- Familiarity with Go structs and interfaces
- No event sourcing knowledge required!

## What is Event Sourcing?

### Traditional Approach (CRUD)

```go
// Store current state
type Task struct {
    ID          string
    Title       string
    Status      string  // "pending", "in_progress", "completed"
    AssignedTo  string
    CompletedAt *time.Time
}

// Update overwrites state
db.Update(&task)  // Lost: who changed it, when, why
```

**Problems**:
- ❌ No history - can't see what changed
- ❌ No audit trail - don't know who did what
- ❌ No time travel - can't see state yesterday
- ❌ Lost intent - why was it changed?

### Event Sourcing Approach

```go
// Store what happened (events)
type TaskCreated struct {
    TaskID      string
    Title       string
    CreatedBy   string
    CreatedAt   time.Time
}

type TaskCompleted struct {
    TaskID      string
    CompletedBy string
    CompletedAt time.Time
}

// Current state = replay all events
```

**Benefits**:
- ✅ Complete history - every change recorded
- ✅ Full audit trail - who, what, when, why
- ✅ Time travel - rebuild state at any point
- ✅ Intent captured - know why changes happened
- ✅ Multiple views - build different read models

## Core Concepts

### 1. Events (What Happened)

Events are **immutable facts** about the past:

```go
type TaskCreated struct {
    TaskID    string
    Title     string
    CreatedAt time.Time
}
```

- Past tense: `TaskCreated`, not `CreateTask`
- Immutable: Never changed after creation
- Facts: Already happened, guaranteed

### 2. Commands (Intent to Change)

Commands represent **intent** to do something:

```go
type CreateTask struct {
    TaskID string
    Title  string
}
```

- Present tense: `CreateTask`, not `TaskCreated`
- Can fail: Business rules might reject
- Validated: Checked before execution

### 3. Aggregates (Business Rules)

Aggregates enforce **business logic**:

```go
func decide(state taskState, cmd CreateTask) ([]Event, error) {
    if state.Exists {
        return nil, errors.New("task already exists")
    }
    
    return []Event{
        &TaskCreated{TaskID: cmd.TaskID, Title: cmd.Title},
    }, nil
}
```

### 4. Projections (Read Models)

Projections build **views from events**:

```go
func evolve(state *TaskList, event Event) *TaskList {
    switch e := event.(type) {
    case *TaskCreated:
        state.Tasks = append(state.Tasks, Task{
            ID:    e.TaskID,
            Title: e.Title,
        })
    }
    return state
}
```

## The Flow

```
┌─────────────┐
│   Command   │  Intent: "Create Task"
│ CreateTask  │
└──────┬──────┘
       │
       ▼
┌─────────────────┐
│ Business Rules  │  Can we do this?
│    (decide)     │  Validate command
└──────┬──────────┘
       │ ✓ Valid
       ▼
┌─────────────┐
│    Event    │  Fact: "Task Was Created"
│ TaskCreated │
└──────┬──────┘
       │
       ├────────────────────┐
       │                    │
       ▼                    ▼
┌─────────────┐    ┌────────────────┐
│ Event Store │    │   Event Bus    │
│  (Persist)  │    │  (Distribute)  │
└─────────────┘    └────────┬───────┘
                            │
                            ▼
                   ┌────────────────┐
                   │  Projections   │
                   │  (Read Models) │
                   └────────────────┘
```

## Tutorial Structure

### Part 1: Setup & First Command (30 min)
- Create a task
- Learn: Commands, Events, Event Store

### Part 2: First Query (15 min)
- List all tasks
- Learn: Queries, Live Projections

### Part 3: Business Rules (20 min)
- Complete a task
- Learn: State, Business rules, Validation

### Part 4: Cached Projections (25 min)
- Personal task dashboard
- Learn: EventBus, Real-time updates

### Part 5: Composition with Screens (20 min)
- Team member vs Manager views
- Learn: Screens, Orchestration

### Part 6: Background Processing (15 min)
- Auto-archive old tasks
- Learn: Processors, Sagas

### Part 7: Production Ready (20 min)
- Testing, KurrentDB, Deployment

**Total time**: ~2.5 hours hands-on

## What You'll Learn

By the end, you'll understand:

- ✅ Event Sourcing fundamentals
- ✅ CQRS (Command Query Responsibility Segregation)
- ✅ Event-driven architecture
- ✅ Building read models from events
- ✅ Handling business rules with events
- ✅ Background processing with events
- ✅ Testing event-sourced systems

## Ready?

Let's build something!

👉 Continue to [Part 1: Your First Command](./part-01-first-command.md)

---

**💡 Tip**: This tutorial is designed to be consumed section by section. Each part builds on the previous one, but you can also skip to specific sections if you're already familiar with earlier concepts.

**🔗 Resources**:
- [terraskye/eventsourcing on GitHub](https://github.com/terraskye/eventsourcing)
- [Complete example code](https://github.com/terraskye/eventsourcing/tree/master/examples/task-management)
- [Event Sourcing concepts](../concepts/)****