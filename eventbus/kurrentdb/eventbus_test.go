//go:build integration

package kurrentdb_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	kdbbus "github.com/terraskye/eventsourcing/eventbus/kurrentdb"

	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type leakEvent struct{}

func (leakEvent) AggregateID() string { return "leak" }
func (leakEvent) EventType() string   { return "leakEvent" }

var testDB *kurrentdb.Client

// TestMain starts a single kurrentdb container for every test in this
// package, so each test doesn't pay its own container-startup cost.
func TestMain(m *testing.M) {
	cqrs.RegisterEventByType(func() cqrs.Event { return &leakEvent{} })

	ctx := context.Background()
	req := testcontainers.ContainerRequest{
		Image:        "kurrentplatform/kurrentdb:latest",
		ExposedPorts: []string{"2113/tcp"},
		Env: map[string]string{
			"EVENTSTORE_INSECURE":        "true",
			"EVENTSTORE_RUN_PROJECTIONS": "None",
			"EVENTSTORE_MEM_DB":          "true",
			"EVENTSTORE_CLUSTER_SIZE":    "1",
		},
		WaitingFor: wait.ForLog("IS LEADER... SPARTA!").WithStartupTimeout(60 * time.Second),
	}
	kc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("start kurrentdb container: %v", err)
	}
	defer kc.Terminate(ctx) //nolint:errcheck

	host, err := kc.Host(ctx)
	if err != nil {
		log.Fatalf("get host: %v", err)
	}
	port, err := kc.MappedPort(ctx, "2113")
	if err != nil {
		log.Fatalf("get port: %v", err)
	}

	settings, err := kurrentdb.ParseConnectionString(fmt.Sprintf("esdb://%s:%s?tls=false", host, port.Port()))
	if err != nil {
		log.Fatalf("parse connection string: %v", err)
	}
	testDB, err = kurrentdb.NewClient(settings)
	if err != nil {
		log.Fatalf("new client: %v", err)
	}

	os.Exit(m.Run())
}

func countGoroutinesCreatedBy(substr string) int {
	buf := make([]byte, 4<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), substr)
}

// TestSubscribe_CtxWatcherGoroutineLeaksAfterClose is a regression test for
// GitHub issue #29: the goroutine that auto-removes a subscriber blocked on
// <-ctx.Done() alone, which never fires for a long-lived ctx such as
// context.Background() (used by every other test/example in this repo) —
// leaking one goroutine per Subscribe call for the rest of the process's
// life, unaffected by Close.
func TestSubscribe_CtxWatcherGoroutineLeaksAfterClose(t *testing.T) {
	const n = 10
	const createdBy = "created by github.com/terraskye/eventsourcing/eventbus/kurrentdb.(*EventBus).Subscribe"

	bus := kdbbus.NewEventBus(testDB, 50)

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	before := countGoroutinesCreatedBy(createdBy)

	for i := 0; i < n; i++ {
		name := "leak-sub-" + strconv.Itoa(i)
		if err := bus.Subscribe(context.Background(), name, handler); err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
	}

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give any well-behaved goroutines a chance to exit.
	time.Sleep(500 * time.Millisecond)
	runtime.GC()

	after := countGoroutinesCreatedBy(createdBy)

	leaked := after - before
	if leaked > 0 {
		t.Fatalf("expected 0 leaked ctx-watcher goroutines after Close, got %d (before=%d after=%d)", leaked, before, after)
	}
}

// TestSubscribe_WithFilterStreamEmptyAlwaysFails is a regression test for
// GitHub issue #47: WithFilterStream unconditionally set
// Filter.Prefixes = streams with no handling for an empty/nil slice. An
// empty stream list produced a SubscriptionFilter with both Prefixes and
// Regex empty, which the vendor KurrentDB client rejects outright when
// creating the persistent subscription ("must provide regex or prefixes"),
// so every Subscribe call passing WithFilterStream(nil) failed
// unconditionally instead of being treated as "no filter".
func TestSubscribe_WithFilterStreamEmptyAlwaysFails(t *testing.T) {
	bus := kdbbus.NewEventBus(testDB, 50)

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	err := bus.Subscribe(context.Background(), "filter-stream-empty-sub", handler, kdbbus.WithFilterStream(nil))
	if err != nil {
		t.Fatalf("Subscribe with an empty stream filter should succeed (treated as no filter), got error: %v", err)
	}
}

// TestUse_RacesWithSubscribe is a regression test for GitHub issue #30: Use
// appended to b.middlewares with no synchronization, and Subscribe read
// b.middlewares before acquiring b.mu, so calling Use concurrently with
// Subscribe was a data race under `go test -race`.
//
// The bus is closed up front so Subscribe bails out right after building the
// wrapped handler (reading b.middlewares) and before it ever touches the nil
// KurrentDB client — this isolates the middlewares race from needing a live
// server.
func TestUse_RacesWithSubscribe(t *testing.T) {
	bus := kdbbus.NewEventBus(nil, 10)

	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			bus.Use(func(next cqrs.EventHandler) cqrs.EventHandler {
				return next
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = bus.Subscribe(context.Background(), "sub", handler)
		}
	}()

	wg.Wait()
}

// TestWithFilterEvents_EmptySliceMeansCatchAll is a regression test for
// GitHub issue #46: WithFilterEvents built the server-side filter regex as
// fmt.Sprintf("^(%s)$", strings.Join(filteredEvents, "|")) with no special
// case for an empty list, producing the literal regex "^()$" — which
// matches only the empty string, so a subscriber configured with a nil or
// empty filter never received a single event, silently. Every other
// EventBus implementation in this module treats an empty filter as "no
// filtering — deliver everything".
func TestWithFilterEvents_EmptySliceMeansCatchAll(t *testing.T) {
	opts := &kurrentdb.PersistentAllSubscriptionOptions{}
	kdbbus.WithFilterEvents(nil)(opts)

	if opts.Filter == nil {
		// No filter at all is also a valid way to mean "catch-all" — the
		// fix takes this path, leaving opts.Filter unset.
		return
	}

	re, err := regexp.Compile(opts.Filter.Regex)
	if err != nil {
		t.Fatalf("built an invalid regex %q: %v", opts.Filter.Regex, err)
	}

	for _, eventType := range []string{"OrderCreated", "ItemAdded", "leakEvent"} {
		if !re.MatchString(eventType) {
			t.Errorf("empty filter should match every event type (catch-all, matching the "+
				"memory/file/postgres EventBus convention), but regex %q does not match %q",
				opts.Filter.Regex, eventType)
		}
	}
}

// TestClose_SendsSpuriousErrorOnNormalShutdown is a probe for whether a
// perfectly normal Close() call - not any real subscription failure -
// still reports an error on Errors(), because runSubscription discards the
// real reason a persistent-subscription Recv() unblocks
// (subscriptionEvent.SubscriptionDropped.Error, which is context.Canceled
// here) and replaces it with a generic
// errors.New("subscription dropped, reconnecting"), which runSubscriber
// then reports as if reconnection attempts had been exhausted.
func TestClose_SendsSpuriousErrorOnNormalShutdown(t *testing.T) {

	bus := kdbbus.NewEventBus(testDB, 10)

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	name := fmt.Sprintf("close-spurious-err-sub-%d", time.Now().UnixNano())
	if err := bus.Subscribe(context.Background(), name, handler); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Let the subscriber goroutine actually reach its blocking Recv() call
	// before we cancel it, matching the realistic case of shutting down a
	// live, idle subscription.
	time.Sleep(1 * time.Second)

	done := make(chan error, 1)
	go func() { done <- bus.Close() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("Close did not return in time")
	}

	select {
	case err, ok := <-bus.Errors():
		if ok {
			t.Fatalf("expected no error on Errors() after a normal Close, got: %v", err)
		}
		t.Log("Errors() channel closed with nothing buffered - no spurious error")
	default:
		t.Fatalf("Errors() channel should be closed (and readable without blocking) once Close has returned")
	}
}

// TestSubscribe_EnsureSubscriptionErrorDeadlocksBus is a regression test
// documenting a bug in EventBus.Subscribe: when EnsurePersistentSubscription
// returns a non-nil error, Subscribe's error-handling branch
//
//	if err := b.EnsurePersistentSubscription(ctx, name, opt); err != nil {
//		cancel()
//		b.errs <- err
//		return err
//	}
//
// returns without ever calling b.mu.Unlock() — b.mu was locked at the top of
// Subscribe and is only unlocked on the success path. Every later call that
// needs b.mu (another Subscribe, Use, or Close) on that same bus instance
// then blocks forever.
//
// EnsurePersistentSubscription returns a non-nil error whenever
// CreatePersistentSubscriptionToAll fails after GetPersistentSubscriptionInfoToAll
// reported the subscription didn't exist yet — the ordinary outcome of losing
// a create race against another instance subscribing under the same name at
// the same time, e.g. two replicas of a service starting up concurrently.
// This test reproduces that race against a real KurrentDB server.
func TestSubscribe_EnsureSubscriptionErrorDeadlocksBus(t *testing.T) {

	handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, event cqrs.Event) error {
		return nil
	})

	const attempts = 8
	for attempt := 0; attempt < attempts; attempt++ {
		name := fmt.Sprintf("deadlock-race-sub-%d-%d", time.Now().UnixNano(), attempt)

		bus1 := kdbbus.NewEventBus(testDB, 10)
		bus2 := kdbbus.NewEventBus(testDB, 10)

		var wg sync.WaitGroup
		var err1, err2 error
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			err1 = bus1.Subscribe(context.Background(), name, handler)
		}()
		go func() {
			defer wg.Done()
			<-start
			err2 = bus2.Subscribe(context.Background(), name, handler)
		}()
		close(start)
		wg.Wait()

		var failedBus *kdbbus.EventBus
		switch {
		case err1 != nil && err2 == nil:
			failedBus = bus1
		case err2 != nil && err1 == nil:
			failedBus = bus2
		default:
			// Both succeeded (race window missed) or both failed (unexpected) —
			// clean up and retry under a fresh name.
			closeWithTimeout(t, bus1)
			closeWithTimeout(t, bus2)
			continue
		}

		t.Logf("attempt %d: reproduced create race (err1=%v, err2=%v)", attempt, err1, err2)

		// If the bug is present, failedBus.mu was left locked by Subscribe's
		// error path, so Close (which also needs b.mu) hangs forever.
		done := make(chan error, 1)
		go func() { done <- failedBus.Close() }()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Close: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Close deadlocked: Subscribe's EnsurePersistentSubscription " +
				"error path never released the bus mutex (see .bug/ report)")
		}
		return
	}

}

// closeWithTimeout closes bus, failing the test instead of hanging forever if
// the bug this file documents makes Close block.
func closeWithTimeout(t *testing.T, bus *kdbbus.EventBus) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- bus.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Close deadlocked during cleanup")
	}
}

// TestWithFilterEvents_MetacharactersInEventTypeAreNotEscaped verifies
// WithFilterEvents' doc comment promise of delivery restricted to "specific
// event types" from filteredEvents holds even when an entry contains a
// regex metacharacter (a very common naming convention: dotted names like
// "order.created") — each entry must be escaped via regexp.QuoteMeta before
// being joined into the server-side filter regex
// (fmt.Sprintf("^(%s)$", strings.Join(...))), so "." is matched as a
// literal character rather than treated as a wildcard that would silently
// widen the filter to match event types the caller never asked for.
func TestWithFilterEvents_MetacharactersInEventTypeAreNotEscaped(t *testing.T) {
	opts := &kurrentdb.PersistentAllSubscriptionOptions{}
	kdbbus.WithFilterEvents([]string{"order.created"})(opts)

	re, err := regexp.Compile(opts.Filter.Regex)
	if err != nil {
		t.Fatalf("built an invalid regex %q: %v", opts.Filter.Regex, err)
	}

	// A literal filter for "order.created" must not match "orderXcreated" —
	// the two differ at the 6th character ('.' vs 'X') and a subscriber
	// filtering on the exact event type "order.created" should never
	// receive an "orderXcreated" event.
	if re.MatchString("orderXcreated") {
		t.Errorf("WithFilterEvents([]string{%q}) produced regex %q, which incorrectly "+
			"matches %q as if '.' were a wildcard instead of a literal character",
			"order.created", opts.Filter.Regex, "orderXcreated")
	}
}
