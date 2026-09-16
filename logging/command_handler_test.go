package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
)

// newCapturingLogger returns a logger that writes one JSON record per line
// into the returned buffer, at debug level so every message the middlewares in
// this package emit is captured. It is shared by the package's tests.
func newCapturingLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(handler), &buf
}

// logRecords parses the JSON lines a logger from newCapturingLogger wrote.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parse log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// requireDuration asserts that rec carries a duration field, which slog's JSON
// handler writes as a nanosecond count, and returns it.
func requireDuration(t *testing.T, rec map[string]any) time.Duration {
	t.Helper()

	raw, ok := rec["duration"]
	if !ok {
		t.Fatalf("log record %v has no duration field", rec)
	}
	ns, ok := raw.(float64)
	if !ok {
		t.Fatalf("duration = %#v (%[1]T), want a number of nanoseconds", raw)
	}
	if ns < 0 {
		t.Fatalf("duration = %v, want a non-negative number of nanoseconds", ns)
	}
	return time.Duration(ns)
}

type testCommand struct {
	ID string
}

func (c testCommand) AggregateID() string { return c.ID }

// TestWithCommandLogging_LogsSuccessWithDuration covers the gap this
// middleware used to have: it logged before running the handler but nothing
// after it succeeded, so a slow command left no trace of how long it took.
func TestWithCommandLogging_LogsSuccessWithDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	const handlerDelay = 2 * time.Millisecond
	handler := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		time.Sleep(handlerDelay)
		return eventsourcing.AppendResult{Successful: true, StreamID: "order-1", NextExpectedVersion: 7}, nil
	})

	if _, err := handler(context.Background(), testCommand{ID: "order-1"}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2 (one before the call, one after): %v", len(recs), recs)
	}

	if recs[0]["msg"] != "Dispatch" {
		t.Errorf("first msg = %v, want %q", recs[0]["msg"], "Dispatch")
	}
	if recs[0]["command"] != "logging.testCommand" {
		t.Errorf("first command = %v, want %q", recs[0]["command"], "logging.testCommand")
	}
	if recs[0]["aggregateID"] != "order-1" {
		t.Errorf("first aggregateID = %v, want %q", recs[0]["aggregateID"], "order-1")
	}

	done := recs[1]
	if done["level"] != "INFO" {
		t.Errorf("completion level = %v, want INFO", done["level"])
	}
	if done["msg"] != "Dispatch succeeded" {
		t.Errorf("completion msg = %v, want %q", done["msg"], "Dispatch succeeded")
	}
	if done["command"] != "logging.testCommand" {
		t.Errorf("completion command = %v, want %q", done["command"], "logging.testCommand")
	}
	if done["aggregateID"] != "order-1" {
		t.Errorf("completion aggregateID = %v, want %q", done["aggregateID"], "order-1")
	}
	if done["streamID"] != "order-1" {
		t.Errorf("completion streamID = %v, want %q", done["streamID"], "order-1")
	}
	if done["version"] != float64(7) {
		t.Errorf("completion version = %v, want 7", done["version"])
	}
	if d := requireDuration(t, done); d < handlerDelay {
		t.Errorf("duration = %v, want at least the %v the handler slept", d, handlerDelay)
	}
}

// TestWithCommandLogging_BusinessRuleViolationLogsWarn asserts that a rejected
// command is not reported as a system failure, matching the otel package,
// which marks such a span Ok rather than failed.
func TestWithCommandLogging_BusinessRuleViolationLogsWarn(t *testing.T) {
	logger, buf := newCapturingLogger()

	cause := errors.New("seat already taken")
	handler := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{Successful: false}, eventsourcing.NewBusinessRuleViolation(cause)
	})

	_, err := handler(context.Background(), testCommand{ID: "order-1"})
	if err == nil {
		t.Fatal("handler returned no error")
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "WARN" {
		t.Errorf("level = %v, want WARN — a violated business rule is an expected outcome, not a failure", done["level"])
	}
	if done["msg"] != "Dispatch rejected" {
		t.Errorf("msg = %v, want %q", done["msg"], "Dispatch rejected")
	}
	if done["reason"] != cause.Error() {
		t.Errorf("reason = %v, want %q", done["reason"], cause.Error())
	}
	requireDuration(t, done)

	for _, rec := range recs {
		if rec["level"] == "ERROR" {
			t.Errorf("logged an ERROR record for a business rule violation: %v", rec)
		}
	}
}

// TestWithCommandLogging_ConflictLogsRevisions asserts that an
// optimistic-concurrency conflict — what a command whose retries ran out ends
// with — carries the revisions needed to diagnose it.
func TestWithCommandLogging_ConflictLogsRevisions(t *testing.T) {
	logger, buf := newCapturingLogger()

	conflict := &eventsourcing.StreamRevisionConflictError{
		Stream:           "order-1",
		ExpectedRevision: eventsourcing.Revision(3),
		ActualRevision:   eventsourcing.Revision(5),
	}
	handler := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{Successful: false}, conflict
	})

	if _, err := handler(context.Background(), testCommand{ID: "order-1"}); !errors.Is(err, error(conflict)) {
		t.Fatalf("handler returned %v, want the conflict unchanged", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", done["level"])
	}
	if done["msg"] != "Dispatch failed" {
		t.Errorf("msg = %v, want %q", done["msg"], "Dispatch failed")
	}
	if done["conflict"] != "stream revision" {
		t.Errorf("conflict = %v, want %q", done["conflict"], "stream revision")
	}
	if done["streamID"] != "order-1" {
		t.Errorf("streamID = %v, want %q", done["streamID"], "order-1")
	}
	if done["expectedRevision"] != float64(3) {
		t.Errorf("expectedRevision = %v, want 3", done["expectedRevision"])
	}
	if done["actualRevision"] != float64(5) {
		t.Errorf("actualRevision = %v, want 5", done["actualRevision"])
	}
	requireDuration(t, done)
}

// TestWithCommandLogging_ConflictWithoutRevisions guards the logger against a
// conflict error whose StreamState fields were left nil: rendering it must not
// panic inside the middleware.
func TestWithCommandLogging_ConflictWithoutRevisions(t *testing.T) {
	logger, buf := newCapturingLogger()

	handler := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, &eventsourcing.StreamRevisionConflictError{Stream: "order-1"}
	})

	if _, err := handler(context.Background(), testCommand{ID: "order-1"}); err == nil {
		t.Fatal("handler returned no error")
	}

	recs := logRecords(t, buf)
	done := recs[len(recs)-1]
	if done["expectedRevision"] != nil {
		t.Errorf("expectedRevision = %v, want null for a nil StreamState", done["expectedRevision"])
	}
	if done["actualRevision"] != nil {
		t.Errorf("actualRevision = %v, want null for a nil StreamState", done["actualRevision"])
	}
}

// TestWithCommandLogging_ErrorLogsDuration asserts an ordinary failure is
// still logged at error level, now with the duration alongside it.
func TestWithCommandLogging_ErrorLogsDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	wantErr := errors.New("store unreachable")
	handler := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, wantErr
	})

	if _, err := handler(context.Background(), testCommand{ID: "order-1"}); !errors.Is(err, wantErr) {
		t.Fatalf("handler returned %v, want %v", err, wantErr)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", done["level"])
	}
	if done["msg"] != "Dispatch failed" {
		t.Errorf("msg = %v, want %q", done["msg"], "Dispatch failed")
	}
	if done["error"] != wantErr.Error() {
		t.Errorf("error = %v, want %q", done["error"], wantErr.Error())
	}
	requireDuration(t, done)
}

// TestCommandLogging_MiddlewareLogsCompletion asserts the bus-wide middleware
// form logs the same completion record as the wrapper it delegates to.
func TestCommandLogging_MiddlewareLogsCompletion(t *testing.T) {
	logger, buf := newCapturingLogger()

	middleware := CommandLogging(logger)
	handler := middleware(func(ctx context.Context, cmd eventsourcing.Command) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{Successful: true, StreamID: "order-9", NextExpectedVersion: 1}, nil
	})

	if _, err := handler(context.Background(), testCommand{ID: "order-9"}); err != nil {
		t.Fatalf("handler: %v", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}
	if recs[1]["msg"] != "Dispatch succeeded" {
		t.Errorf("completion msg = %v, want %q", recs[1]["msg"], "Dispatch succeeded")
	}
	if recs[1]["streamID"] != "order-9" {
		t.Errorf("completion streamID = %v, want %q", recs[1]["streamID"], "order-9")
	}
	requireDuration(t, recs[1])
}
