package projectiontest_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/terraskye/eventsourcing/memory"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/projection/projectiontest"
)

// TestStoreAcceptance_FailsInsteadOfHanging covers a store whose Commit
// returns an error without running fn: the Reset/WaitsForCommit subtest
// fails instead of hanging until the test binary times out. It runs the
// suite in a child process, so the failure it expects does not fail this
// test.
func TestStoreAcceptance_FailsInsteadOfHanging(t *testing.T) {
	// failingCommitStore is a non-conforming store: Commit on segment 1
	// fails before it runs fn. The suite should report that as a failure.
	if os.Getenv("PROJECTIONTEST_INNER") != "" {
		projectiontest.StoreAcceptanceTest(t, failingCommitStore{memory.NewProjectionStore()})
		return
	}
	// Run the suite's Reset/WaitsForCommit subtest in a child process with
	// a short timeout, so a hang shows up as a timeout instead of stalling.
	cmd := exec.Command(os.Args[0],
		"-test.run=^TestStoreAcceptance_FailsInsteadOfHanging$/^Reset$/^WaitsForCommit$",
		"-test.timeout=10s", "-test.v")
	cmd.Env = append(os.Environ(), "PROJECTIONTEST_INNER=1")
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), "panic: test timed out") {
		t.Fatalf("StoreAcceptanceTest hung in Reset/WaitsForCommit instead of failing the subtest when Commit returned an error without running fn:\n%s", firstLines(string(out), 25))
	}
	if !strings.Contains(string(out), "--- FAIL: TestStoreAcceptance_FailsInsteadOfHanging/Reset/WaitsForCommit") {
		t.Fatalf("want the subtest to fail; output:\n%s", out)
	}
}

type failingCommitStore struct{ *memory.ProjectionStore }

func (s failingCommitStore) Commit(ctx context.Context, l projection.Lease, cp projection.Checkpoint, st projection.Status, fn func(context.Context) error) error {
	if l.Segment == 1 {
		return errors.New("store unavailable")
	}
	return s.ProjectionStore.Commit(ctx, l, cp, st, fn)
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
