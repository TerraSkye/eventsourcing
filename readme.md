# TerraSkye Eventsourcing

`eventsourcing` is a generic, type-safe event sourcing framework for Go.  
It provides the building blocks for **event-driven architectures**, including command handling, query handling, event buses, envelopes, and a flexible iterator for read models.

This library focuses on simplicity, modern Go patterns, and full generics support, making it easy to build event-sourced systems with strong type guarantees.

---

## Features

- **Commands & Command Handlers** – Strongly typed command routing.
- **Events & Event Handlers** – Publish and consume events safely.
- **Event Bus** – Supports multiple subscribers with typed handlers.
- **Queries & Query Handlers** – Request data through a type-safe query bus.
- **Query Gateway** – Simple façade to dispatch typed queries.
- **Generic Iterator** – Lazy, paginated, or buffered read model iteration.
- **Revision Management** – Built-in support for aggregate and stream revisions.
- **Metadata & Envelopes** – Rich event metadata included by default.
- **Type-Safe Generics Everywhere** – Commands, events, queries, handlers, results.

---

## Installation

```bash
go get github.com/terraskye/eventsourcing
````

