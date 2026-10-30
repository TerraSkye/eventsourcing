# How to retry when you cannot tell whether an append landed

Some errors leave it unknown whether the events were stored. Find out before you append again, so the events are not stored twice.

## When the outcome is unknown

An error from `Save` or `Dispatch` usually means nothing was stored: a revision conflict, a business-rule violation, an unregistered event. A few errors do not say:

- **`Dispatch` returns a context error after the handler started.** `Dispatch` stops waiting when its context is cancelled or times out, but the handler carries on, and may save its events after `Dispatch` has returned. A command whose context ends while it is still queued does not run.
- **The database connection fails during `COMMIT`** on Postgres or SQLite, or the commit times out. The commit may have been applied before the error reached you.
- **A KurrentDB append fails after its retries**, or its context ends while one is in flight. The KurrentDB store retries transient errors itself, with the same event IDs, but the last attempt may still have been written.

In each case the events may be in the store, and a blind retry could add them a second time.

## Retry a Save

Every envelope passed to `Save` carries its `EventID`; a store returns `ErrInvalidEventBatch` for one without. Keep the envelopes of the first attempt, IDs included, and retry with the same expected stream state:

```go
res, err := store.Save(ctx, envs, eventsourcing.Revision(n))
if err != nil && outcomeUnknown(err) {
    res, err = store.Save(ctx, envs, eventsourcing.Revision(n))
}
```

What the retry returns depends on the store and the stream state:

| Retry returns | Means |
|---|---|
| `nil` | Stored, by this call or, on KurrentDB, by the first one: KurrentDB answers a retry with the same event IDs and stream state with success, and stores the events once |
| A `*StreamRevisionConflictError` for a `Revision`, or `ErrStreamExists` for `NoStream` | The stream moved on, either by the first attempt or by another writer. Look for the first event's ID, below |
| Another error | Handle it as usual |

Under `Any` there is no stream state to conflict with: Postgres, SQLite and the memory store append the events a second time. Look for the event ID before retrying a `Save` under `Any`.

### Look for the event ID

Read the stream from the revision you saved at (`Revision(0)` after `NoStream`), and check for the first envelope's `EventID`:

```go
func stored(ctx context.Context, store eventsourcing.EventStore, streamID string, n uint64, id uuid.UUID) (bool, error) {
    it, err := store.LoadStreamFrom(ctx, streamID, eventsourcing.Revision(n))
    if err != nil {
        return false, err
    }
    defer it.Close()
    for it.Next() {
        if it.Value().EventID == id {
            return true, nil
        }
    }
    return false, it.Err()
}
```

A batch is stored whole or not at all, so one ID answers for all of it. Found: the first attempt landed, and there is nothing to retry. Not found: another writer moved the stream on; reload and decide again.

## Retry a command

`NewCommandHandler` gives every call new `EventID`s, so a second `Dispatch` of the same command cannot be recognized by its IDs. It does load the stream again, with any events the first attempt stored, and runs `decide` on that state. A retry is safe when `decide` refuses to do the same thing twice:

- A command that creates something once is safe already: `decide` sees the stream exists and returns a business-rule violation.
- A command that adds to a stream (record a payment, add an item) is not. Give the command an ID of its own, have `evolve` keep the IDs it has seen, and let `decide` return no events for one it has seen:

```go
type AddItem struct {
    CartID    uuid.UUID
    RequestID uuid.UUID // chosen by the caller, the same on every retry
    SKU       string
}

func decide(s cartState, cmd AddItem) ([]eventsourcing.Event, error) {
    if s.Requests[cmd.RequestID] {
        return nil, nil // already applied
    }
    return []eventsourcing.Event{&ItemAdded{CartID: cmd.CartID, RequestID: cmd.RequestID, SKU: cmd.SKU}}, nil
}
```

The request ID goes in the event, so the next load sees it.

See also [Handle concurrency conflicts](./handle-concurrency.md) for the conflicts a retry can run into.
