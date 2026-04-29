# QueryBus & QueryGateway

## QueryBus

```go
type QueryBus struct { ... }

func NewQueryBus() *QueryBus
```

Central registry for query handlers. Handlers are keyed by the pair `(QueryType, ResultType)`.

### RegisterQueryHandler

```go
func RegisterQueryHandler[T Query, R any](
    bus     *QueryBus,
    handler QueryHandler[T, R],
    opts    ...HandlerOption,
)
```

Registers a handler for query type `T` returning result type `R`.

```go
bus := eventsourcing.NewQueryBus()
eventsourcing.RegisterQueryHandler(bus, listTasksHandler)
```

### Validate

```go
func (q QueryBus) Validate() error
```

Returns an error listing any query types that have been requested (via `NewQueryGateway`) but not registered. Call this at startup to catch misconfiguration early.

```go
if err := bus.Validate(); err != nil {
    log.Fatal(err)
}
```

---

## QueryHandler

```go
type QueryHandler[T Query, R any] interface {
    HandleQuery(ctx context.Context, qry T) (R, error)
}
```

Generic interface for query handlers. Implement this on your read model structs, or use `NewQueryHandlerFunc`.

### NewQueryHandlerFunc

```go
func NewQueryHandlerFunc[T Query, R any](
    fn func(ctx context.Context, qry T) (R, error),
) QueryHandler[T, R]
```

Wraps a plain function as a `QueryHandler`:

```go
handler := eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, q GetTask) (*Task, error) {
    return store.Find(q.TaskID)
})
eventsourcing.RegisterQueryHandler(bus, handler)
```

---

## QueryGateway

```go
type QueryGateway[T Query, R any] func(ctx context.Context, qry T) (R, error)

func NewQueryGateway[T Query, R any](bus *QueryBus) QueryGateway[T, R]
```

Typed, callable facade over `QueryBus`. Call it directly like a function. It also implements `QueryHandler[T, R]`, so it can be passed to decorators such as `WithQueryTelemetry` or `WithQueryLogging`.

```go
gateway := eventsourcing.NewQueryGateway[GetTask, *Task](bus)

task, err := gateway(ctx, GetTask{TaskID: id})
```

Creating a gateway registers the `(T, R)` key as a "requestee", which is checked by `bus.Validate()`.

Because `QueryGateway` is a function type, a service struct can hold multiple gateways for the same query type with different result shapes — each independently registered on the bus:

```go
type service struct {
    listTasks eventsourcing.QueryGateway[TaskQuery, *Iterator[*Task]]
    findTask  eventsourcing.QueryGateway[TaskQuery, *Task]
}
```

---

## Typical setup

```go
// 1. Create bus
bus := eventsourcing.NewQueryBus()

// 2. Register handlers
eventsourcing.RegisterQueryHandler(bus, listTasksQueryHandler)
eventsourcing.RegisterQueryHandler(bus, getTaskQueryHandler)

// 3. Validate
if err := bus.Validate(); err != nil {
    log.Fatal(err)
}

// 4. Create gateways for use in HTTP handlers
listGateway := eventsourcing.NewQueryGateway[ListTasks, *TaskList](bus)
getGateway  := eventsourcing.NewQueryGateway[GetTask, *Task](bus)

// Call directly — no .HandleQuery needed
tasks, err := listGateway(ctx, ListTasks{})
task,  err := getGateway(ctx, GetTask{TaskID: id})
```
