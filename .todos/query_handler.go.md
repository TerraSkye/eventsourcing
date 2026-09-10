# TODO: `query_handler.go`

51 lines. `Query`, `QueryHandler[T, R]`, and the func adapter.

## API
- [ ] **`Query.ID() []byte` should be a string.** It is used purely as an identifier for logs and
      traces, and every consumer immediately converts it: `string(qry.ID())` appears in
      `logging/query_handler.go:25`, `otel/query_handler.go:29` and `:115`. Each conversion allocates
      and copies. A `[]byte` is also mutable, so a caller can alter an ID another goroutine is
      reading. `ID() string` removes the allocations and the aliasing.

## Good
- `queryHandlerFunc` adapter is the standard `http.HandlerFunc` pattern, correctly applied.
- The doc example on `QueryHandler` includes the `var _ QueryHandler[MyQuery, *MyResult] = handler`
  assertion, which shows readers how to check their own wiring.
