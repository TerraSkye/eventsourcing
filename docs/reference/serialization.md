# Serialization

Every store keeps events as JSON, written and read through four functions in the root package. There is no other encoding and no codec option: this page is the whole contract.

## The functions

```go
func EncodeEvent(event Event) ([]byte, error)
func DecodeEvent(name string, data []byte) (Event, error)
func EncodeMetadata(metadata map[string]any) ([]byte, error)
func DecodeMetadata(data []byte) (map[string]any, error)
```

- `EncodeEvent` returns `event` encoded by `encoding/json`. It fails with an error wrapping `ErrEventNotRegistered` unless the event's type is registered under `event.EventType()`, so an event a store could not read back fails when it is saved.
- `DecodeEvent` makes a new instance of the type registered under `name` and decodes `data` into it with `encoding/json`. An unknown name is `ErrEventNotRegistered`.
- `EncodeMetadata` encodes an envelope's `Metadata`; a nil map encodes as `{}`.
- `DecodeMetadata` decodes it back. Empty data and `null` decode as an empty map, never nil.

## What a stored event is

A store keeps, for each event:

| Part | Value |
|---|---|
| Name | `event.EventType()` at the time of `Save` |
| Payload | `EncodeEvent(event)` |
| Metadata | `EncodeMetadata(envelope.Metadata)` |

Reading an event is `DecodeEvent(name, payload)` and `DecodeMetadata(metadata)`. The other envelope fields (`EventID`, `StreamID`, `Version`, `GlobalPosition`, `OccurredAt`, `CorrelationID`, `CausationID`) are kept in the store's own columns or fields, not in the JSON.

## What follows for event types

- **Every event type is registered**, in every store, the in-memory one included. See [Register events](../how-to/register-events.md).
- **Reads return a pointer.** `RegisterEvent(&TaskCreated{})` registers a factory for `*TaskCreated`, so every read returns `*TaskCreated`, even for an event saved as a value. Type switches and assertions use the pointer type.
- **The JSON is the event's JSON.** Struct tags, `json.Marshaler` and `json.Unmarshaler` on the event type all apply. To read an older shape of an event, give the type an `UnmarshalJSON` method; see [Read an event's old shape](../how-to/reshape-events-with-unmarshaljson.md).
- **Metadata comes back as JSON decodes it**: numbers as `float64`, objects as `map[string]any`, arrays as `[]any`. A value of your own type comes back as one of those, not as your type.

## Per store

| Store | Payload and metadata are kept as |
|---|---|
| `memory` | The encoded bytes, decoded again on every read, so each read returns its own copy |
| `sqlite` | The encoded bytes, in `BLOB` columns |
| `postgres` | The encoded bytes, in `BYTEA` columns |
| `kurrentdb` | The event's data, with content type JSON, and its user metadata. The metadata also carries the envelope's chain, under KurrentDB's `$correlationId` and `$causationId` keys |

## Writing a store

A store outside this module is byte-compatible with the ones in it when it:

1. Calls `EncodeEvent` and `EncodeMetadata` for every event before writing any of a batch, and fails the whole `Save` if one fails.
2. Stores the payload under `event.EventType()`, the name `EncodeEvent` checked.
3. Reads with `DecodeEvent` and `DecodeMetadata`.
4. Stores the `EventID` each envelope carries, and rejects an envelope without one with an error wrapping `ErrInvalidEventBatch`, as [`EventStore.Save`](./interfaces.md#eventstore) describes.

`eventsourcingtest.AcceptanceTest` checks a store against the `EventStore` contract, round trip of event and metadata included.
