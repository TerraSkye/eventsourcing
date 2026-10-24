package telemetry

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
	"go.opentelemetry.io/otel/attribute"
)

// projectedEvent is the event the projection tests project.
type projectedEvent struct{}

func (*projectedEvent) AggregateID() string { return "agg-1" }
func (*projectedEvent) EventType() string   { return "otel_test.projected" }

func init() {
	eventsourcing.RegisterEvent(&projectedEvent{})
}

// newProjectionName returns a projection name no other test uses, so the
// test can pick its own records out of the shared recorders.
func newProjectionName() string {
	return "otel-test-" + uuid.NewString()
}

// forProjection returns the records for name, keeping the order they were
// recorded in.
func forProjection(records []histRecord, name string) []histRecord {
	var out []histRecord
	for _, rec := range records {
		if v, ok := rec.attrs.Value(AttrProjectionName); ok && v.AsString() == name {
			out = append(out, rec)
		}
	}
	return out
}

func attrString(set attribute.Set, key attribute.Key) string {
	v, _ := set.Value(key)
	return v.AsString()
}

// TestProjectionTelemetry_Runner runs a projection over three events with
// every telemetry wrapper in place, and checks the batch span wraps the
// handler spans, which carry the projection's name, and that the metrics
// report it live and caught up.
func TestProjectionTelemetry_Runner(t *testing.T) {
	name := newProjectionName()
	stub := &stubStore{}
	for i := 1; i <= 3; i++ {
		stub.events = append(stub.events, &eventsourcing.Envelope{
			StreamID: "agg-1", Event: &projectedEvent{}, Version: uint64(i), GlobalPosition: uint64(i),
		})
	}
	reader := WithEventStoreTelemetry(stub).(eventsourcing.GlobalReader)
	handler := WithEventTelemetry(eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		return nil
	}))
	r := projection.NewRunner(projection.Definition{
		Name:         name,
		Handler:      handler,
		EventTypes:   []string{"otel_test.projected"},
		PollInterval: 10 * time.Millisecond,
	}, reader, WithProjectionStoreTelemetry(memory.NewProjectionStore()))
	reg, err := ObserveProjection(r)
	if err != nil {
		t.Fatalf("ObserveProjection: %v", err)
	}
	t.Cleanup(func() { _ = reg.Unregister() })

	before := spanNameRecorder.len()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		_ = r.Run(ctx)
		close(done)
	}()
	waitCtx, waitCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer waitCancel()
	if err := r.WaitUntil(waitCtx, 3); err != nil {
		t.Fatalf("WaitUntil(3): %v", err)
	}
	for r.Status().Phase != projection.Live {
		if waitCtx.Err() != nil {
			t.Fatalf("Status().Phase = %s, want %s", r.Status().Phase, projection.Live)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Read the gauges while the runner is live: once Run returns, it no
	// longer processes the projection and reports Standby.
	observed := map[string]float64{}
	for _, rec := range forProjection(durationRecorder.collect(t.Context()), name) {
		key := rec.name
		if phase := attrString(rec.attrs, AttrProjectionPhase); phase != "" {
			key += "/" + phase
		}
		observed[key] = rec.value
	}
	cancel()
	<-done

	var names []string
	var handlerSpans int
	for _, s := range spanNameRecorder.spansSince(before) {
		if s.name == "process event" {
			if got := attrString(s.attrs, AttrProjectionName); got == name {
				handlerSpans++
			}
		}
		if attrString(s.attrs, AttrProjectionName) == name {
			names = append(names, s.name)
		}
	}
	if want := []string{"process batch", "process event", "process event", "process event"}; !slices.Equal(names[:min(len(names), 4)], want) {
		t.Errorf("spans for the projection = %v, want them to start with %v", names, want)
	}
	if handlerSpans != 3 {
		t.Errorf("handler spans tagged with the projection = %d, want 3", handlerSpans)
	}

	batches := forProjection(durationRecorder.recordsFor("eventsourcing.projection.batch.duration"), name)
	if len(batches) == 0 || attrString(batches[0].attrs, AttrResult) != "success" {
		t.Errorf("batch durations = %v, want the first with result success", batches)
	}

	want := map[string]float64{
		"eventsourcing.projection.lag":               0,
		"eventsourcing.projection.position":          3,
		"eventsourcing.projection.events.skipped":    0,
		"eventsourcing.projection.phase/starting":    0,
		"eventsourcing.projection.phase/standby":     0,
		"eventsourcing.projection.phase/catching_up": 0,
		"eventsourcing.projection.phase/live":        1,
		"eventsourcing.projection.phase/stalled":     0,
	}
	for k, v := range want {
		if got, ok := observed[k]; !ok || got != v {
			t.Errorf("observed %s = %v (present %v), want %v", k, got, ok, v)
		}
	}
}

// TestWithProjectionStoreTelemetry_Claim checks every Claim is counted by
// its result.
func TestWithProjectionStoreTelemetry_Claim(t *testing.T) {
	name := newProjectionName()
	store := WithProjectionStoreTelemetry(memory.NewProjectionStore())
	if err := store.Declare(t.Context(), name, 1, 0); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	for _, owner := range []string{"a", "b"} {
		if _, _, _, err := store.Claim(t.Context(), projection.Lease{Name: name, Owner: owner}, time.Minute); err != nil {
			t.Fatalf("Claim(%q): %v", owner, err)
		}
	}

	var results []string
	for _, rec := range forProjection(durationRecorder.recordsFor("eventsourcing.projection.claims"), name) {
		results = append(results, attrString(rec.attrs, AttrClaimResult))
	}
	if want := []string{"acquired", "held"}; !slices.Equal(results, want) {
		t.Errorf("claim results = %v, want %v", results, want)
	}
}

// TestWithProjectionStoreTelemetry_Heartbeat checks a heartbeat records
// the live instances and the segments its own instance holds, and that
// the ttl-0 heartbeat an instance leaves with is not counted.
func TestWithProjectionStoreTelemetry_Heartbeat(t *testing.T) {
	name := newProjectionName()
	store := WithProjectionStoreTelemetry(memory.NewProjectionStore())
	if err := store.Declare(t.Context(), name, 3, 0); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	for seg := range 2 {
		if _, _, _, err := store.Claim(t.Context(), projection.Lease{Name: name, Segment: seg, Owner: "a"}, time.Minute); err != nil {
			t.Fatalf("Claim(%d): %v", seg, err)
		}
	}
	for _, owner := range []string{"b", "a"} {
		if _, err := store.Heartbeat(t.Context(), name, owner, 0, time.Minute); err != nil {
			t.Fatalf("Heartbeat(%q): %v", owner, err)
		}
	}
	if _, err := store.Heartbeat(t.Context(), name, "b", 0, 0); err != nil {
		t.Fatalf("Heartbeat(b, ttl 0): %v", err)
	}

	values := func(metric string) []float64 {
		var out []float64
		for _, rec := range forProjection(durationRecorder.recordsFor(metric), name) {
			out = append(out, rec.value)
		}
		return out
	}
	if got, want := values("eventsourcing.projection.heartbeats"), []float64{1, 1}; !slices.Equal(got, want) {
		t.Errorf("heartbeats = %v, want %v", got, want)
	}
	if got, want := values("eventsourcing.projection.instance.count"), []float64{1, 2}; !slices.Equal(got, want) {
		t.Errorf("instance counts = %v, want %v", got, want)
	}
	if got, want := values("eventsourcing.projection.segment.held"), []float64{0, 2}; !slices.Equal(got, want) {
		t.Errorf("segments held = %v, want %v", got, want)
	}
}

// TestWithProjectionStoreTelemetry_CommitClaimLost checks a Commit by an
// instance that does not hold the segment is recorded as claim_lost.
func TestWithProjectionStoreTelemetry_CommitClaimLost(t *testing.T) {
	name := newProjectionName()
	store := WithProjectionStoreTelemetry(memory.NewProjectionStore())
	if err := store.Declare(t.Context(), name, 1, 0); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	err := store.Commit(t.Context(), projection.Lease{Name: name, Owner: "a"}, projection.Checkpoint{Position: 1}, projection.Status{},
		func(context.Context) error { return nil })
	if !errors.Is(err, projection.ErrClaimLost) {
		t.Fatalf("Commit without a claim = %v, want ErrClaimLost", err)
	}

	batches := forProjection(durationRecorder.recordsFor("eventsourcing.projection.batch.duration"), name)
	if len(batches) != 1 || attrString(batches[0].attrs, AttrResult) != "claim_lost" {
		t.Errorf("batch durations = %v, want one with result claim_lost", batches)
	}
}

// TestWithProjectionStoreTelemetry_Reset checks Reset runs in a "rebuild
// projection" span and is counted.
func TestWithProjectionStoreTelemetry_Reset(t *testing.T) {
	name := newProjectionName()
	store := WithProjectionStoreTelemetry(memory.NewProjectionStore())
	l := projection.Lease{Name: name, Owner: "a"}
	if err := store.Declare(t.Context(), name, 1, 0); err != nil {
		t.Fatalf("Declare: %v", err)
	}
	l, _, ok, err := store.Claim(t.Context(), l, time.Minute)
	if err != nil || !ok {
		t.Fatalf("Claim = %v, %v; want true, nil", ok, err)
	}

	before := spanNameRecorder.len()
	if err := store.Reset(t.Context(), l, 2, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if got := spanNameRecorder.since(before); !slices.Equal(got, []string{"rebuild projection"}) {
		t.Errorf("spans started by Reset = %v, want [rebuild projection]", got)
	}
	rebuilds := forProjection(durationRecorder.recordsFor("eventsourcing.projection.rebuilds"), name)
	if len(rebuilds) != 1 || attrString(rebuilds[0].attrs, AttrResult) != "success" {
		t.Errorf("rebuilds = %v, want one with result success", rebuilds)
	}
}

// plainStore is a projection.Store that records no dead letters: only the
// Store methods of the wrapped store are promoted.
type plainStore struct {
	projection.Store
}

// deadLetterStore is a projection.Store that also records dead letters.
type deadLetterStore struct {
	*memory.ProjectionStore
	positions []uint64
}

func (s *deadLetterStore) SaveDeadLetter(_ context.Context, _ projection.Lease, d projection.DeadLetter) error {
	s.positions = append(s.positions, d.Position)
	return nil
}

// TestWithProjectionStoreTelemetry_DeadLetterStore checks the wrapper is a
// DeadLetterStore exactly when the wrapped store is, and forwards to it.
func TestWithProjectionStoreTelemetry_DeadLetterStore(t *testing.T) {
	plain := plainStore{memory.NewProjectionStore()}
	if _, ok := WithProjectionStoreTelemetry(plain).(projection.DeadLetterStore); ok {
		t.Errorf("WithProjectionStoreTelemetry(%T) implements projection.DeadLetterStore, want only projection.Store", plain)
	}

	inner := &deadLetterStore{ProjectionStore: memory.NewProjectionStore()}
	dls, ok := WithProjectionStoreTelemetry(inner).(projection.DeadLetterStore)
	if !ok {
		t.Fatalf("WithProjectionStoreTelemetry(%T) does not implement projection.DeadLetterStore", inner)
	}
	if err := dls.SaveDeadLetter(t.Context(), projection.Lease{Name: "x"}, projection.DeadLetter{Position: 7, StreamID: "agg-1", EventType: "otel_test.projected", Error: "boom"}); err != nil {
		t.Fatalf("SaveDeadLetter: %v", err)
	}
	if !slices.Equal(inner.positions, []uint64{7}) {
		t.Errorf("wrapped store saved dead letters at %v, want [7]", inner.positions)
	}
}

// TestWithEventTelemetry_RecordsErrors checks a failing handler is counted
// in EventBusErrors, tagged with the projection when it runs in a batch.
func TestWithEventTelemetry_RecordsErrors(t *testing.T) {
	name := newProjectionName()
	boom := errors.New("boom")
	handler := WithEventTelemetry(eventsourcing.NewEventHandlerFunc(func(context.Context, eventsourcing.Event) error {
		return boom
	}))
	ctx := context.WithValue(t.Context(), leaseKey{}, projection.Lease{Name: name})
	if err := handler.Handle(ctx, &projectedEvent{}); !errors.Is(err, boom) {
		t.Fatalf("Handle() = %v, want %v", err, boom)
	}

	if errs := forProjection(durationRecorder.recordsFor("eventsourcing.eventbus.errors"), name); len(errs) != 1 {
		t.Errorf("eventbus errors for the projection = %d, want 1", len(errs))
	}
}

// TestEventStore_ReadAllEmptyPageNotTraced checks a read that returns no
// events records no span, while one that returns events does.
func TestEventStore_ReadAllEmptyPageNotTraced(t *testing.T) {
	stub := &stubStore{events: []*eventsourcing.Envelope{
		{StreamID: "agg-1", Event: stubEvent{}, Version: 1, GlobalPosition: 1},
	}}
	reader := WithEventStoreTelemetry(stub).(eventsourcing.GlobalReader)

	before := spanNameRecorder.len()
	if _, err := reader.ReadAll(t.Context(), 1, eventsourcing.ReadOptions{}); err != nil {
		t.Fatalf("ReadAll(after 1): %v", err)
	}
	if got := spanNameRecorder.since(before); len(got) != 0 {
		t.Errorf("spans for an empty read = %v, want none", got)
	}

	before = spanNameRecorder.len()
	if _, err := reader.ReadAll(t.Context(), 0, eventsourcing.ReadOptions{}); err != nil {
		t.Fatalf("ReadAll(after 0): %v", err)
	}
	if got := spanNameRecorder.since(before); !slices.Equal(got, []string{"read_all eventstore"}) {
		t.Errorf("spans for a read with events = %v, want [read_all eventstore]", got)
	}
}
