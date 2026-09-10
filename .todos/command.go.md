# TODO: `command.go`

38 lines. The `Command` interface.

## API
- [ ] **`Command` has no self-declared name, unlike `Event`.** `Event` declares `EventType() string`;
      `Command` declares only `AggregateID()`. That asymmetry is the root cause of the registry bugs
      in `command_bus.go` and `otel/command_handler.go`, which all fall back to `fmt.Sprintf("%T")`.
      Adding `CommandName() string` here is the structural fix — see the decision note in
      `command_bus.go.md`, which has the benchmarks and the collision evidence.

## Good
- This is the best-documented file in the repo. The intent-vs-implementation naming table, the
  immutability and self-containment requirements, and the worked `ReserveSeat` example teach the
  domain model rather than just the syntax. Keep this as the template for the other core interfaces.
