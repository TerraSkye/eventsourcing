package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
)

// ---- Stubs ----

type testCommand struct {
	calls *int
}

func (c testCommand) AggregateID() string {
	if c.calls != nil {
		*c.calls++
	}
	return "order-1"
}

func (c testCommand) CommandType() string {
	if c.calls != nil {
		*c.calls++
	}
	return "PlaceOrder"
}

type testEvent struct{}

func (testEvent) AggregateID() string { return "order-1" }
func (testEvent) EventType() string   { return "OrderPlaced" }

type testQuery struct{}

func (testQuery) ID() []byte { return []byte("q-1") }

// newTestLogger returns a JSON logger at level and a function that decodes
// the records it has written.
func newTestLogger(t *testing.T, level slog.Level) (*slog.Logger, func() []map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
	return logger, func() []map[string]any {
		t.Helper()
		var records []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var r map[string]any
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatalf("decode log line %q: %v", line, err)
			}
			records = append(records, r)
		}
		return records
	}
}

// useDefaultLogger installs logger as [slog.Default] for the rest of the test.
func useDefaultLogger(t *testing.T, logger *slog.Logger) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
}

func assertRecord(t *testing.T, r map[string]any, level, msg string, attrs map[string]any) {
	t.Helper()
	if r["level"] != level || r["msg"] != msg {
		t.Errorf("record = %s %q, want %s %q", r["level"], r["msg"], level, msg)
	}
	for k, want := range attrs {
		if got := r[k]; got != want {
			t.Errorf("record %q: %s = %v, want %v", msg, k, got, want)
		}
	}
}

func eventContext() context.Context {
	ctx := eventsourcing.WithEnvelope(context.Background(), &eventsourcing.Envelope{
		EventID:       uuid.New(),
		StreamID:      "order-1",
		Event:         testEvent{},
		Version:       3,
		GlobalVersion: 42,
	})
	return eventsourcing.WithCausation(ctx, "cmd-1")
}

// ---- Command logging ----

func TestWithCommandLogging_Success(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	h := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{Successful: true}, nil
	})

	if _, err := h(context.Background(), testCommand{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := records()
	if len(rs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(rs), rs)
	}
	assertRecord(t, rs[0], "INFO", "dispatching command", map[string]any{
		"command_type": "PlaceOrder",
		"aggregate_id": "order-1",
	})
}

func TestWithCommandLogging_Error(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	h := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, errors.New("boom")
	})

	if _, err := h(context.Background(), testCommand{}); err == nil {
		t.Fatal("expected error")
	}

	rs := records()
	if len(rs) != 2 {
		t.Fatalf("got %d records, want 2: %v", len(rs), rs)
	}
	assertRecord(t, rs[1], "ERROR", "command failed", map[string]any{
		"command_type": "PlaceOrder",
		"aggregate_id": "order-1",
		"error":        "boom",
	})
}

func TestWithCommandLogging_DisabledLevelSkipsAttributes(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelError)
	h := WithCommandLogging(logger, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{Successful: true}, nil
	})

	calls := 0
	if _, err := h(context.Background(), testCommand{calls: &calls}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calls != 0 {
		t.Errorf("command accessors called %d times with info disabled, want 0", calls)
	}
	if rs := records(); len(rs) != 0 {
		t.Errorf("got %d records, want 0: %v", len(rs), rs)
	}
}

func TestWithCommandLogging_NilLoggerUsesDefault(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	useDefaultLogger(t, logger)

	h := WithCommandLogging(nil, func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, nil
	})
	if _, err := h(context.Background(), testCommand{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rs := records(); len(rs) != 1 {
		t.Errorf("got %d records on slog.Default, want 1: %v", len(rs), rs)
	}
}

// ---- Event logging ----

func TestWithEventLogging_Success(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	h := WithEventLogging(logger, eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	}))

	if err := h.Handle(eventContext(), testEvent{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := records()
	if len(rs) != 2 {
		t.Fatalf("got %d records, want 2: %v", len(rs), rs)
	}
	attrs := map[string]any{
		"event_type":     "OrderPlaced",
		"stream_id":      "order-1",
		"aggregate_id":   "order-1",
		"version":        float64(3),
		"global_version": float64(42),
		"causation":      "cmd-1",
	}
	assertRecord(t, rs[0], "DEBUG", "handling event", attrs)
	assertRecord(t, rs[1], "DEBUG", "event handled", attrs)
}

func TestWithEventLogging_ErrorLoggedWithDebugDisabled(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelInfo)
	h := WithEventLogging(logger, eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return errors.New("boom")
	}))

	if err := h.Handle(eventContext(), testEvent{}); err == nil {
		t.Fatal("expected error")
	}

	rs := records()
	if len(rs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(rs), rs)
	}
	assertRecord(t, rs[0], "ERROR", "event failed", map[string]any{
		"event_type": "OrderPlaced",
		"stream_id":  "order-1",
		"error":      "boom",
	})
}

func TestWithEventLogging_NilLoggerUsesDefault(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	useDefaultLogger(t, logger)

	h := WithEventLogging(nil, eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	}))
	if err := h.Handle(eventContext(), testEvent{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rs := records(); len(rs) != 2 {
		t.Errorf("got %d records on slog.Default, want 2: %v", len(rs), rs)
	}
}

// ---- Query logging ----

func TestWithQueryLogging_Success(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	h := WithQueryLogging(logger, eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, qry testQuery) (int, error) {
		return 1, nil
	}))

	if _, err := h.HandleQuery(context.Background(), testQuery{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rs := records()
	if len(rs) != 1 {
		t.Fatalf("got %d records, want 1: %v", len(rs), rs)
	}
	assertRecord(t, rs[0], "INFO", "handling query", map[string]any{
		"query_type": "logging.testQuery",
	})
}

func TestWithQueryLogging_Error(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	h := WithQueryLogging(logger, eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, qry testQuery) (int, error) {
		return 0, errors.New("boom")
	}))

	if _, err := h.HandleQuery(context.Background(), testQuery{}); err == nil {
		t.Fatal("expected error")
	}

	rs := records()
	if len(rs) != 2 {
		t.Fatalf("got %d records, want 2: %v", len(rs), rs)
	}
	assertRecord(t, rs[1], "ERROR", "query failed", map[string]any{
		"query_type": "logging.testQuery",
		"error":      "boom",
	})
}

func TestWithQueryLogging_NilLoggerUsesDefault(t *testing.T) {
	logger, records := newTestLogger(t, slog.LevelDebug)
	useDefaultLogger(t, logger)

	h := WithQueryLogging(nil, eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, qry testQuery) (int, error) {
		return 1, nil
	}))
	if _, err := h.HandleQuery(context.Background(), testQuery{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rs := records(); len(rs) != 1 {
		t.Errorf("got %d records on slog.Default, want 1: %v", len(rs), rs)
	}
}

// ---- Benchmarks: logging disabled for the level ----

func discardLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(discard{}, &slog.HandlerOptions{Level: level}))
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func BenchmarkWithCommandLogging_Disabled(b *testing.B) {
	h := WithCommandLogging(discardLogger(slog.LevelError), func(ctx context.Context, cmd testCommand) (eventsourcing.AppendResult, error) {
		return eventsourcing.AppendResult{}, nil
	})
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h(ctx, testCommand{})
	}
}

func BenchmarkWithEventLogging_Disabled(b *testing.B) {
	h := WithEventLogging(discardLogger(slog.LevelInfo), eventsourcing.NewEventHandlerFunc(func(ctx context.Context, event eventsourcing.Event) error {
		return nil
	}))
	ctx := eventContext()
	b.ReportAllocs()
	for b.Loop() {
		_ = h.Handle(ctx, testEvent{})
	}
}

func BenchmarkWithQueryLogging_Disabled(b *testing.B) {
	h := WithQueryLogging(discardLogger(slog.LevelError), eventsourcing.NewQueryHandlerFunc(func(ctx context.Context, qry testQuery) (int, error) {
		return 1, nil
	}))
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = h.HandleQuery(ctx, testQuery{})
	}
}
