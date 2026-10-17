# Rebuild a projection without downtime

Bumping a projection's `Version` rebuilds it in place: `Reset` empties the read model, and queries see an empty or half-built table until the runner has caught up with the log again. For a small log that takes seconds. For a large one it can take long enough that users notice.

This guide rebuilds the projection **next to** the old one instead. The old version keeps serving queries while the new one catches up, and queries switch over once the new one is live.

**Prerequisite**: a projection stored in Postgres, as in [Build a projection](./cached-projection.md#store-the-read-model-in-postgres).

## When to use which

| | Rebuild in place (`Version` bump) | Rebuild next to it (this guide) |
|---|---|---|
| Read model while rebuilding | empty, then filling up | old version, complete |
| Steps | one deploy | two deploys and a cleanup |
| Extra storage | none | a second copy until cleanup |
| Fits | small logs, internal read models | large logs, read models users depend on |

## How it works

The runner keys everything by `Name`: the checkpoint, the claims and the status. A projection under a new name has no checkpoint, so it starts at the beginning of the log, like a brand-new projection. Give the new version a new name and its own tables, and the two versions never touch each other.

```
deploy 1:   task-list-v1 ──► tasks_v1 ◄── queries
            task-list-v2 ──► tasks_v2      (catching up)

deploy 2:   task-list-v2 ──► tasks_v2 ◄── queries
```

## Step 1: Version the name and the table

Put the version in both the projection's name and its table, so the next version can sit next to this one:

```go
package tasklist

func NewProjectionV2() projection.Definition {
    return projection.Definition{
        Name:       "task-list-v2",
        Handler:    eventsourcing.NewEventHandlerFunc(handleV2),
        EventTypes: []string{"TaskCreated", "TaskCompleted", "TaskRenamed"},
    }
}

func handleV2(ctx context.Context, event eventsourcing.Event) error {
    switch e := event.(type) {
    case *events.TaskCreated:
        _, err := postgres.Tx(ctx).Exec(ctx,
            `INSERT INTO tasks_v2 (id, title) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
            e.TaskID, e.Title)
        return err
    // ...
    }
    return nil
}
```

Create `tasks_v2` with your migrations. The new projection does not need a `Version` or a `Reset`: it starts from an empty table and an empty checkpoint.

## Step 2: Run both versions

Deploy with both runners. The old one keeps the current read model up to date; the new one works through the log from the start:

```go
store := postgres.NewStore(pool)

v1 := projection.NewRunner(tasklist.NewProjection(), eventStore, store)
v2 := projection.NewRunner(tasklist.NewProjectionV2(), eventStore, store)

go v1.Run(ctx)
go v2.Run(ctx)

queries := tasklist.NewQueryHandler(pool) // still reads tasks_v1
```

Both runners read the log, so expect extra load on the event store while `v2` catches up.

While `v2` processes events that were stored before it first started, `projection.IsReplaying(ctx)` is true, so side effects guarded by it do not fire a second time.

## Step 3: Wait until the new version is live

`v2.IsLive(ctx)` reports true once it has reached the head of the log. From outside the service, read the status the runner publishes:

```sql
SELECT segment, phase, position, head
FROM projection_segments
WHERE name = 'task-list-v2';
```

Every segment should report `live`, with `position` close to `head`.

## Step 4: Switch the queries

Point the query at `tasks_v2` and deploy without the `v1` runner:

```go
v2 := projection.NewRunner(tasklist.NewProjectionV2(), eventStore, store)
go v2.Run(ctx)

queries := tasklist.NewQueryHandler(pool) // now reads tasks_v2
```

!!! tip "Switch without a deploy"
    If the query reads a view instead of a table, the switch is one statement, and switching back is just as quick:

    ```sql
    CREATE OR REPLACE VIEW tasks AS SELECT * FROM tasks_v2;
    ```

    Keep the `v1` runner running until you are sure you will not switch back.

## Step 5: Clean up

Once nothing reads the old version, drop its table and its rows in the projection store:

```sql
DROP TABLE tasks_v1;
DELETE FROM projection_segments WHERE name = 'task-list-v1';
DELETE FROM projection_dead_letters WHERE projection = 'task-list-v1';
```

## The same approach for other changes

A new name is also the way to change things that are fixed per name:

- **The segment count.** `Segments` cannot change under the same name; build the new count under a new name.
- **The segment key.** Changing `SegmentKey` moves events to other segments, so their checkpoints no longer match. Treat it like a new segment count.
