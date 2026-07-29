//go:build integration

package kurrentdb_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
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
