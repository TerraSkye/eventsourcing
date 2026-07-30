package kurrentdb_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	kdbstore "github.com/terraskye/eventsourcing/eventstore/kurrentdb"

	"github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type CheckEvent struct {
	N int
}

func (CheckEvent) AggregateID() string { return "check" }
func (CheckEvent) EventType() string   { return "CheckEvent" }

var testDB *kurrentdb.Client

// TestMain starts a single kurrentdb container for every test in this
// package, so each test doesn't pay its own container-startup cost.
func TestMain(m *testing.M) {
	cqrs.RegisterEventByType(func() cqrs.Event { return &CheckEvent{} })

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

func collectAll(t *testing.T, iter *cqrs.Iterator[*cqrs.Envelope]) []*cqrs.Envelope {
	t.Helper()
	var out []*cqrs.Envelope
	for iter.Next(context.Background()) {
		out = append(out, iter.Value())
	}
	if err := iter.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

// TestLoadStreamFrom_RevisionIsInclusiveNotExclusive is a regression test
// for GitHub issue #45: LoadStreamFrom(ctx, id, cqrs.Revision(N)) read
// starting at revision N inclusive, because it forwarded N directly into
// kurrentdb.StreamRevision{Value: N} — KurrentDB's own native,
// inclusive-of-N read position. Every other EventStore implementation in
// this repo treats Revision(N) as "N events already consumed, resume
// strictly after N" (exclusive), which is exactly the contract
// NewCommandHandler's retry loop depends on: resuming a load from a
// previously-seen revision re-delivered the last-consumed event a second
// time, corrupting aggregate state by evolving it twice.
func TestLoadStreamFrom_RevisionIsInclusiveNotExclusive(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-inclusive"

	// Save 3 events (native KurrentDB revisions 0, 1, 2).
	for i := 0; i < 3; i++ {
		_, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID:   streamID,
			Event:      &CheckEvent{N: i},
			Metadata:   map[string]any{},
			OccurredAt: time.Now(),
		}}, cqrs.Any{})
		if err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	// Simulate NewCommandHandler's evolve loop: consume everything once and
	// remember the last event's Version, exactly as command_handler.go does
	// via `revision = Revision(event.Version)`.
	iter, err := store.LoadStreamFrom(ctx, streamID, cqrs.Any{})
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	all := collectAll(t, iter)
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}
	lastVersion := all[len(all)-1].Version // native EventNumber = 2

	// Retry: resume from the last consumed revision. Since all 3 events
	// (indices 0,1,2) were already evolved, this MUST yield zero events.
	iter, err = store.LoadStreamFrom(ctx, streamID, cqrs.Revision(lastVersion))
	if err != nil {
		t.Fatalf("resume load: %v", err)
	}
	resumed := collectAll(t, iter)
	if len(resumed) != 0 {
		t.Errorf("expected 0 events when resuming from the last consumed revision %d, got %d: re-delivered event(s) %v",
			lastVersion, len(resumed), resumed)
	}
}

// TestLoadFromAll_HangsPastLastEvent is a regression test for GitHub issue
// #44: LoadFromAll passed count=0 to (*kurrentdb.Client).ReadAll, unlike
// LoadStream/LoadStreamFrom in the same file, which both pass 5000. Against
// a real server, count=0 does not bound the read — once the iterator
// consumed every currently-existing event, Next blocked forever instead of
// returning false.
func TestLoadFromAll_HangsPastLastEvent(t *testing.T) {
	store := kdbstore.NewEventStore(testDB)
	ctx := context.Background()
	streamID := "check-loadfromall-hangs"

	// Save 3 events so the $all stream has a known non-empty tail.
	for i := 0; i < 3; i++ {
		if _, err := store.Save(ctx, []cqrs.Envelope{{
			StreamID:   streamID,
			Event:      &CheckEvent{N: i},
			Metadata:   map[string]any{},
			OccurredAt: time.Now(),
		}}, cqrs.Any{}); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}

	iter, err := store.LoadFromAll(ctx, cqrs.Any{})
	if err != nil {
		t.Fatalf("load from all: %v", err)
	}

	// Drain every event currently in the $all stream. Once exhausted, Next
	// MUST return false (with iter.Err() == nil) rather than blocking forever.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for iter.Next(ctx) {
		}
	}()

	select {
	case <-done:
		// The point of this test is that the drain loop terminates at all
		// (the bug was that it never did). $all also carries KurrentDB's own
		// internal events (e.g. "$metadata"), which this store's registry
		// was never meant to decode, so a non-EOF error here is expected and
		// not itself a failure — only a timeout is.
		if err := iter.Err(); err != nil && !errors.Is(err, io.EOF) {
			t.Logf("iterator ended with a non-EOF error (expected for unregistered system events in $all): %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("LoadFromAll's iterator did not terminate within 15s after exhausting the $all stream: " +
			"Next() blocks forever instead of returning false, because LoadFromAll passes count=0 to " +
			"(*kurrentdb.Client).ReadAll (eventstore.go)")
	}
}
