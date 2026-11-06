# Personal data and events

An event store is append-only: once an event is saved, it stays, unchanged, for as long as the log does. Personal data has to be correctable and erasable. The two meet in one rule: **keep personal data out of events, and refer to people by ID.**

## Reference people by ID

An event records that something happened and who it happened to, by ID:

```go
// Good: who, by ID
type TaskAssigned struct {
    TaskID     uuid.UUID `json:"task_id"`
    AssigneeID uuid.UUID `json:"assignee_id"`
}

// Not this: the person themselves
type TaskAssigned struct {
    TaskID        uuid.UUID `json:"task_id"`
    AssigneeName  string    `json:"assignee_name"`
    AssigneeEmail string    `json:"assignee_email"`
}
```

The name and email live where they can be changed and deleted: a user service, a table of your own, an identity provider. A read model that shows names looks them up by ID, or keeps its own copy that it can update and delete. Erasing a person is then a delete in that one place. The events keep an ID that no longer resolves to anyone, and the history they record (a task was assigned, then completed) stays intact.

The same goes for **metadata**. `WithMetadataExtractor` stamps every event a handler saves, so it is the easiest place for personal data to spread unnoticed: record `user_id`, not the user's name, email address or IP address. Metadata is stored with the event, as permanent as its payload.

Watch **free text** too. A task title or a comment is whatever the user typed, and that can be a phone number. Payloads are stored as plain JSON, which anyone with database access can read and search. If a field may hold personal data, keep its text outside the event as well, and put its ID in the event.

## Why there is no crypto-shredding

Crypto-shredding encrypts each person's data in the events with a key of their own, and erases them by deleting the key. The library does not offer it, for these reasons:

- **Every read needs the keys.** Loading an aggregate, running or rebuilding a projection and replaying a dead letter all decrypt. A key store becomes a dependency of every read, and its outage stops all of them.
- **A deleted key changes history.** An aggregate that folds an event whose fields now decode as empty may decide differently than it did, and a rebuilt read model no longer matches the one it replaced.
- **It does not reach the copies.** Read models, caches, search indexes, logs and backups hold the data in the clear, and need erasing on their own anyway.

Keeping personal data out of events avoids all three: there is nothing in the log to erase.

If you need it anyway, it fits at the edges of the library rather than inside it: an event type's `MarshalJSON` and `UnmarshalJSON` can encrypt and decrypt its own fields (see [Serialization](../reference/serialization.md)), with the trade-offs above.
