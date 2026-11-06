# How to read an event's old shape with UnmarshalJSON

Let an event type read the events it was stored as before a field changed, by giving it an `UnmarshalJSON` method. There is no upcaster chain in the library: this is the extension point for a change in shape.

## When to use this, and when to version

Use `UnmarshalJSON` when the event still means the same thing and only its JSON changed: a field was renamed, a field was added and old events need a default, one field became two, two became one, or a value is spelled differently.

When the meaning changes, introduce a new event type instead, as [Version events](./version-events.md) describes. A consumer can then tell the old fact from the new one, which an `UnmarshalJSON` shim deliberately hides.

## Write the method

Every store decodes an event with `encoding/json` (see [Serialization](../reference/serialization.md)), so a pointer-receiver `UnmarshalJSON` on the event type runs for every stored event of that type. Decode into a copy of the struct that has no such method, next to the old fields, then fill in what the old shape left out:

```go
type TaskCreated struct {
    TaskID          string `json:"task_id"`
    Title           string `json:"title"`
    Priority        string `json:"priority"`
    Width           int    `json:"width"`
    Height          int    `json:"height"`
    EstimateMinutes int    `json:"estimate_minutes"`
    Status          string `json:"status"`
}

func (e *TaskCreated) UnmarshalJSON(data []byte) error {
    type current TaskCreated // the same fields, without this method
    var v struct {
        current
        Name          string `json:"name"`           // renamed to title
        Size          string `json:"size"`           // "WxH", split into width and height
        EstimateHours int    `json:"estimate_hours"` // folded into estimate_minutes
    }
    if err := json.Unmarshal(data, &v); err != nil {
        return err
    }
    *e = TaskCreated(v.current)

    // Renamed field: take the old name when the new one is absent.
    if e.Title == "" {
        e.Title = v.Name
    }
    // Added field: a default for events written before it existed.
    if e.Priority == "" {
        e.Priority = "normal"
    }
    // Split field: one old value becomes two new ones.
    if w, h, ok := strings.Cut(v.Size, "x"); ok {
        e.Width, _ = strconv.Atoi(w)
        e.Height, _ = strconv.Atoi(h)
    }
    // Combined field: two old values become one new one.
    e.EstimateMinutes += v.EstimateHours * 60
    // Remapped value: an old spelling becomes the current one.
    if e.Status == "done" {
        e.Status = "completed"
    }
    return nil
}
```

`type current TaskCreated` matters: decoding into `TaskCreated` itself would call this method again and recurse forever. The conversion back, `TaskCreated(v.current)`, works because the two types have the same fields.

An event stored as

```json
{"task_id":"t1","name":"Write docs","size":"3x2","estimate_hours":1,"estimate_minutes":30,"status":"done"}
```

decodes to `TaskCreated{TaskID: "t1", Title: "Write docs", Priority: "normal", Width: 3, Height: 2, EstimateMinutes: 90, Status: "completed"}`, and an event in the current shape decodes unchanged.

No `MarshalJSON` is needed. New events are written in the current shape, so the old shape only ever exists in events already stored, and the method has to keep reading it for as long as those events do.

## Rename the type too

If the Go type or its stored name changed as well, map the old name to the new type with `RegisterEventByName`; its `UnmarshalJSON` then reads the old events under either name:

```go
func init() {
    eventsourcing.RegisterEvent(&TaskCreated{})                 // "TaskCreated", the current name
    eventsourcing.RegisterEventByName("task.created", func() eventsourcing.Event {
        return &TaskCreated{}                                   // events stored before the rename
    })
}
```

See [Register events](./register-events.md).

## Test it with a stored payload

Keep a payload in the old shape in a test, and decode it the way a store does:

```go
func TestTaskCreated_ReadsTheOldShape(t *testing.T) {
    ev, err := eventsourcing.DecodeEvent("TaskCreated",
        []byte(`{"task_id":"t1","name":"Write docs","status":"done"}`))
    if err != nil {
        t.Fatal(err)
    }
    got := ev.(*TaskCreated)
    if got.Title != "Write docs" || got.Status != "completed" {
        t.Errorf("decoded %+v", got)
    }
}
```

The test keeps the shim honest after the struct changes again.
