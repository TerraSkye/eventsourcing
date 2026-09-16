package eventsourcing

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// QueryBus is a central registry of query handlers, keyed by their query
// and result types, so that multiple query types can be registered on a
// single bus. Handlers are executed through a typed [QueryGateway] created
// with [NewQueryGateway].
//
// Example Usage:
//
//	bus := NewQueryBus()
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
type QueryBus struct {
	mu          sync.RWMutex
	handlers    map[string]registeredQuery
	requestees  map[string]struct{}
	middlewares []QueryHandlerMiddleware
}

// NewQueryBus creates a new, empty QueryBus ready for handler registration.
func NewQueryBus() *QueryBus {
	return &QueryBus{
		handlers:   make(map[string]registeredQuery),
		requestees: make(map[string]struct{}),
	}
}

// HandlerOption configures a handler being registered on a [QueryBus]. See
// [WithQueryTimeout]; further options may be added for concerns such as worker
// pools or rate limiting.
type HandlerOption func(*handlerSettings)

// handlerSettings stores internal configuration for a registered handler.
type handlerSettings struct {
	// timeout is the default deadline applied to every query dispatched to
	// this handler, or zero for none. See [WithQueryTimeout].
	timeout time.Duration
}

// WithQueryTimeout gives the handler being registered a default deadline:
// every query dispatched to it runs under a context that expires after d, and
// the [QueryGateway] stops waiting once it does rather than blocking on a
// handler that may never return.
//
// d is a ceiling, not an override — a caller whose own context expires sooner
// still wins. A d of zero or less registers no default deadline, which is what
// registering without this option does.
//
// Example Usage:
//
//	RegisterQueryHandlerFunc(bus, store.ListTasks, WithQueryTimeout(2*time.Second))
func WithQueryTimeout(d time.Duration) HandlerOption {
	return func(settings *handlerSettings) {
		settings.timeout = d
	}
}

// registeredQuery is what a [QueryBus] holds for one (query, result) type
// pair: the middleware-wrapped handler, kept as an any because its type
// parameters are not known here, alongside the settings its [HandlerOption]s
// produced at registration.
type registeredQuery struct {
	handler  any
	settings handlerSettings
}

// queryKey returns the registry key for query type T and result type R. It
// formats (*T)(nil) and (*R)(nil) — not fmt.Sprintf("%T", *new(R)): the
// latter erases R's static type when R is an interface, since *new(R) is
// then a nil interface value carrying no dynamic type, and %T renders every
// such value as the same "<nil>" string regardless of R. A pointer to R does
// not have this problem: *R is a concrete pointer type in its own right even
// when R is an interface, so %T reports it correctly (e.g. "*io.Reader")
// without needing reflect.TypeOf to recover R's static type explicitly.
func queryKey[T Query, R any]() string {
	return fmt.Sprintf("%T|%T", (*T)(nil), (*R)(nil))
}

// RegisterQueryHandlerFunc registers a plain function as a query handler.
// Type parameters are inferred from the function signature. Prefer this over
// RegisterQueryHandler when registering method values from a provider struct.
//
// Panics if a handler for the same query and result types is already registered.
//
// Example Usage:
//
//	RegisterQueryHandlerFunc(bus, store.GetTask)
//	RegisterQueryHandlerFunc(bus, store.ListTasks)
func RegisterQueryHandlerFunc[T Query, R any](bus *QueryBus, fn queryHandlerFunc[T, R], opts ...HandlerOption) {
	RegisterQueryHandler(bus, fn, opts...)
}

// RegisterQueryHandler registers a QueryHandler[T, R] on the bus. Use this
// when registering a type that explicitly implements the QueryHandler interface.
// For plain functions or method values, prefer RegisterQueryHandlerFunc.
//
// Panics if a handler for the same query and result types is already registered.
//
// Example Usage:
//
//	RegisterQueryHandler(bus, myHandler)
func RegisterQueryHandler[T Query, R any](bus *QueryBus, handler QueryHandler[T, R], opts ...HandlerOption) {
	key := queryKey[T, R]()

	bus.mu.Lock()
	defer bus.mu.Unlock()

	if _, exists := bus.handlers[key]; exists {
		panic(ErrDuplicateHandler)
	}

	settings := handlerSettings{}
	for _, opt := range opts {
		opt(&settings)
	}

	bus.handlers[key] = registeredQuery{
		handler:  wrapQueryHandler[T, R](handler, bus.middlewares),
		settings: settings,
	}
}

// Validate reports an error listing every query/result type pair that a
// [QueryGateway] was created for via [NewQueryGateway] but that has no
// registered handler. Call it during startup, after all gateways and
// handlers are wired up, to catch a missing registration before it can
// surface as a runtime error.
func (q *QueryBus) Validate() error {
	q.mu.RLock()
	defer q.mu.RUnlock()

	errs := make([]error, 0)
	for requestee := range q.requestees {
		if _, ok := q.handlers[requestee]; !ok {
			errs = append(errs, fmt.Errorf("unknown query handler: %s", requestee))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// addRequestee registers key as a requestee under bus.mu, so it is safe to
// call concurrently with itself, [RegisterQueryHandler], and [QueryBus.Validate].
func (q *QueryBus) addRequestee(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.requestees[key] = struct{}{}
}

// handlerFor returns the handler registered for key together with its
// settings, and reports whether one exists. The lock is held only for the map
// lookup, never for the handler call, so a slow handler cannot block
// RegisterQueryHandler.
func (q *QueryBus) handlerFor(key string) (registeredQuery, bool) {
	q.mu.RLock()
	defer q.mu.RUnlock()
	h, ok := q.handlers[key]
	return h, ok
}
