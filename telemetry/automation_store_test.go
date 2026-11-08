package telemetry

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/memory"
	"go.opentelemetry.io/otel/codes"
)

// TestWithAutomationStoreTelemetry_Segments checks acquisitions, grants,
// the waiting gauge and lost leases.
func TestWithAutomationStoreTelemetry_Segments(t *testing.T) {
	ctx := t.Context()
	name := newAutomationName()
	store := WithAutomationStoreTelemetry(memory.NewAutomationStore())
	cands := []automation.Segment{{Key: "a"}, {Key: "b"}, {Key: "c"}}
	res, err := store.Acquire(ctx, name, "o1", cands, 2, 50*time.Millisecond)
	if err != nil || len(res.Granted) != 2 {
		t.Fatalf("Acquire = %+v, %v; want two grants", res, err)
	}
	time.Sleep(100 * time.Millisecond) // the leases expire
	if lost, err := store.Renew(ctx, res.Granted, time.Minute); err != nil || len(lost) != 2 {
		t.Fatalf("Renew = lost %v, %v; want both", lost, err)
	}

	sum := func(metric string) (total float64, results []string) {
		for _, rec := range forAutomation(durationRecorder.recordsFor(metric), name) {
			total += rec.value
			if r := attrString(rec.attrs, AttrResult); r != "" {
				results = append(results, r)
			}
		}
		return total, results
	}
	if n, results := sum("eventsourcing.automation.acquisitions"); n != 1 || !slices.Equal(results, []string{"success"}) {
		t.Errorf("acquisitions = %v %v, want one success", n, results)
	}
	if n, _ := sum("eventsourcing.automation.segment.grants"); n != 2 {
		t.Errorf("grants = %v, want 2", n)
	}
	if n, _ := sum("eventsourcing.automation.segment.waiting"); n != 1 {
		t.Errorf("waiting = %v, want 1: c had work and no owner", n)
	}
	if n, _ := sum("eventsourcing.automation.segment.lost"); n != 2 {
		t.Errorf("lost = %v, want 2", n)
	}
}

// TestWithAutomationStoreTelemetry_PauseSpans checks that pausing and
// resuming are traced, and that the spans' status is left unset when they
// succeed.
func TestWithAutomationStoreTelemetry_PauseSpans(t *testing.T) {
	name := newAutomationName()
	store := WithAutomationStoreTelemetry(memory.NewAutomationStore())
	before := spanNameRecorder.len()
	if err := store.SetPaused(t.Context(), name, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPaused(t.Context(), name, false); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range spanNameRecorder.spansSince(before) {
		if attrString(s.attrs, AttrAutomationName) == name {
			got = append(got, s.name)
			if s.status != codes.Unset {
				t.Errorf("span %s has status %v, want Unset", s.name, s.status)
			}
		}
	}
	sort.Strings(got)
	if want := []string{"pause automation", "resume automation"}; !slices.Equal(got, want) {
		t.Errorf("spans = %v, want %v", got, want)
	}
	if p, _ := store.Paused(t.Context(), name); p {
		t.Error("the automation is still paused")
	}
}
