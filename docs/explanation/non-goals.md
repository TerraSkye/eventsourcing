# What the library does not do

Some features are left out on purpose. Each entry says why, and where the door is left open if that ever changes.

| Not doing | Why | The door left open |
|---|---|---|
| Pluggable codecs, per-store serialization options | One encoding, set in one place, so four stores cannot drift apart | `EncodeEvent` / `DecodeEvent` is the single seam if that ever changes |
| Protobuf, MessagePack, CBOR, gob | They conflict with plain Go structs as events, and with reading stored events as JSON; gob is not stable across releases | none |
| An upcaster chain | Versioned event types are the stance, and each consumer decides what an old version means. A change in shape only is handled by the type itself with [`UnmarshalJSON`](../how-to/reshape-events-with-unmarshaljson.md) | A `RegisterUpcaster[From, To](fn)` applied inside `DecodeEvent` would not break anything |
| Serialized context values in events | Correlation and causation are first-class `Envelope` fields, and `WithEnvelope(ctx)` rebuilds the rest for a handler | none |
| Compression | Payloads are small, and Postgres compresses large column values (TOAST) anyway | none |
| Crypto-shredding, field-level encryption of personal data | Keep personal data out of events and metadata, and refer to people by ID; see [Personal data and events](./personal-data.md) | It would sit on `EncodeEvent` / `DecodeEvent` and projection rebuilds |
| An instance (non-global) event registry | Simplicity: one process, one set of event types | none |
| `encoding/json/v2` | Still behind `GOEXPERIMENT=jsonv2` in Go 1.26, and its defaults change what stored events mean | A deliberate release with the `json/v1` compatibility options |
| An event bus, push subscriptions | `projection.Runner` reads the same events with checkpoints, retries, rebuilds and segments; v0.5.0 removed the bus. See [Move from the event bus](../how-to/move-off-the-event-bus.md) | `Runner.Nudge` wakes a runner without waiting for its poll |

Asking for one of these is still welcome: open an issue with the use case, and say what the workaround above does not cover.
