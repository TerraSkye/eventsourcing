# Tutorials: Build a Task Management API

These tutorials teach event sourcing by building a **Task Management API** from scratch using `terraskye/eventsourcing` and Gin.

## What you'll build

A task management system with:

- Commands: create tasks, assign them, mark as complete
- Queries: list all tasks, personal dashboard
- Real-time projections that update automatically
- Background processing to auto-archive completed tasks

## Prerequisites

- Go 1.21+ installed
- Basic understanding of Go structs and interfaces
- Familiarity with HTTP APIs
- No event sourcing knowledge required

## Tutorial parts

| Part | Topic | What you learn | Time |
|------|-------|----------------|------|
| [1](./01-first-command.md) | Your first command | Commands, events, event store | 30 min |
| [2](./02-first-query.md) | Your first query | Queries, live read models | 15 min |
| [3](./03-business-rules.md) | Business rules | State, the decide/evolve pattern | 20 min |
| [4](./04-projections.md) | Real-time projections | Event bus, cached projections | 25 min |
| [5](./05-background-processing.md) | Background processing | Processors, sagas | 15 min |

**Total time**: ~1.5 hours hands-on

## Project structure

Throughout the tutorials you'll build this layout:

```
task-management/
├── main.go
├── events/
│   ├── task_created.go
│   ├── task_assigned.go
│   └── task_completed.go
├── slices/
│   ├── createtask/
│   │   ├── command.go
│   │   └── http.go
│   ├── completetask/
│   │   ├── command.go
│   │   └── http.go
│   └── listtasks/
│       ├── query.go
│       └── http.go
└── go.mod
```

This follows [Vertical Slice Architecture](../explanation/vertical-slice-architecture.md), where each feature is self-contained.

## Initial setup

Create the project:

```bash
mkdir task-management && cd task-management
go mod init task-management
go get github.com/terraskye/eventsourcing
go get github.com/terraskye/eventsourcing/eventstore/memory
go get github.com/gin-gonic/gin
go get github.com/google/uuid
```

Ready? Start with [Part 1: Your First Command](./01-first-command.md).
