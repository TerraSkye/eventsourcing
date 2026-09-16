package eventsourcing

import (
	"context"
	"fmt"
)

// QueryGateway is a typed, callable facade over a [QueryBus]. Call it
// directly like a function to execute the handler registered for query type
// T and result type R. It also implements [QueryHandler], so it can be
// passed to decorators such as WithQueryTelemetry or WithQueryLogging.
//
// Example Usage:
//
//	gateway := NewQueryGateway[MyQuery, *MyResult](bus)
//	result, err := gateway(ctx, MyQuery{ID: "42"})
type QueryGateway[T Query, R any] func(ctx context.Context, qry T) (R, error)

// HandleQuery implements [QueryHandler] by calling g.
func (g QueryGateway[T, R]) HandleQuery(ctx context.Context, qry T) (R, error) {
	return g(ctx, qry)
}

// GenericQueryGateway is a backwards-compatible alias for [QueryGateway].
//
// Deprecated: use QueryGateway directly.
type GenericQueryGateway[T Query, R any] = QueryGateway[T, R]

// NewQueryGateway returns a [QueryGateway] for query type T and result type
// R, backed by bus. It registers the (T, R) pair on bus as a requestee, so
// that a later call to [QueryBus.Validate] fails if no handler for that pair
// is ever registered.
//
// The returned gateway stops waiting as soon as ctx is done, returning an
// error wrapping [context.Context.Err] instead of blocking on a handler that
// has not come back. A handler registered with [WithQueryTimeout] runs under
// that deadline, or under the caller's own if it is earlier. The handler is
// not interrupted — Go cannot do that — so one that ignores ctx runs to
// completion off to the side and its result is discarded; a handler that
// honours ctx stops on its own.
//
// Example Usage:
//
//	listGateway := NewQueryGateway[ListTasks, *TaskList](bus)
//	findGateway := NewQueryGateway[ListTasks, *Task](bus)
func NewQueryGateway[T Query, R any](bus *QueryBus) QueryGateway[T, R] {
	key := queryKey[T, R]()
	bus.addRequestee(key)

	return func(ctx context.Context, qry T) (R, error) {
		entry, ok := bus.handlerFor(key)
		if !ok {
			var zero R
			return zero, fmt.Errorf("no handler registered for query %T -> %T %w", qry, (*R)(nil), ErrHandlerNotFound)
		}

		handler, ok := entry.handler.(QueryHandler[T, R])
		if !ok {
			var zero R
			return zero, fmt.Errorf("handler type mismatch for query %T -> %T", qry, (*R)(nil))
		}

		if entry.settings.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, entry.settings.timeout)
			defer cancel()
		}

		// Nothing can cut this call short, so run it on the caller's own
		// goroutine and skip the machinery below entirely.
		if ctx.Done() == nil {
			return handler.HandleQuery(ctx, qry)
		}

		// Hand the call to a goroutine so the caller is released the moment
		// ctx is done, the same contract [CommandBus.Dispatch] gives a command
		// still in flight. Go cannot interrupt the handler itself: one that
		// ignores ctx keeps running to completion, and its result is dropped.
		// The channel is buffered so that goroutine can deliver and exit
		// rather than blocking forever on a caller that has already left.
		type outcome struct {
			result R
			err    error
			panic  any
		}
		done := make(chan outcome, 1)

		go func() {
			// Recover so a panicking handler can be re-panicked on the
			// caller's goroutine, where it would have surfaced before the
			// call moved off it. A panic that loses the race to ctx is
			// discarded along with the rest of the abandoned call.
			defer func() {
				if p := recover(); p != nil {
					done <- outcome{panic: p}
				}
			}()

			result, err := handler.HandleQuery(ctx, qry)
			done <- outcome{result: result, err: err}
		}()

		select {
		case o := <-done:
			if o.panic != nil {
				panic(o.panic)
			}
			return o.result, o.err
		case <-ctx.Done():
			var zero R
			return zero, fmt.Errorf("query %T -> %T: %w", qry, (*R)(nil), ctx.Err())
		}
	}
}
