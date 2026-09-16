package logging

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
)

type testQuery struct {
	QueryID string
}

func (q testQuery) ID() []byte { return []byte(q.QueryID) }

type testQueryResult struct {
	Value int
}

// TestWithQueryLogging_LogsSuccessWithDuration covers the gap this middleware
// used to have: it logged before running the handler but nothing after it
// succeeded, so a slow query left no trace of how long it took.
func TestWithQueryLogging_LogsSuccessWithDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	const handlerDelay = 2 * time.Millisecond
	handler := WithQueryLogging(logger, eventsourcing.NewQueryHandlerFunc(
		func(ctx context.Context, qry testQuery) (*testQueryResult, error) {
			time.Sleep(handlerDelay)
			return &testQueryResult{Value: 42}, nil
		},
	))

	result, err := handler.HandleQuery(context.Background(), testQuery{QueryID: "q-1"})
	if err != nil {
		t.Fatalf("HandleQuery: %v", err)
	}
	if result.Value != 42 {
		t.Fatalf("result = %#v, want Value 42", result)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2 (one before the call, one after): %v", len(recs), recs)
	}

	if recs[0]["msg"] != "Query" {
		t.Errorf("first msg = %v, want %q", recs[0]["msg"], "Query")
	}
	if recs[0]["query"] != "logging.testQuery" {
		t.Errorf("first query = %v, want %q", recs[0]["query"], "logging.testQuery")
	}
	if recs[0]["queryID"] != "q-1" {
		t.Errorf("first queryID = %v, want %q", recs[0]["queryID"], "q-1")
	}

	done := recs[1]
	if done["level"] != "INFO" {
		t.Errorf("completion level = %v, want INFO", done["level"])
	}
	if done["msg"] != "Query succeeded" {
		t.Errorf("completion msg = %v, want %q", done["msg"], "Query succeeded")
	}
	if done["query"] != "logging.testQuery" {
		t.Errorf("completion query = %v, want %q", done["query"], "logging.testQuery")
	}
	if done["queryID"] != "q-1" {
		t.Errorf("completion queryID = %v, want %q", done["queryID"], "q-1")
	}
	if d := requireDuration(t, done); d < handlerDelay {
		t.Errorf("duration = %v, want at least the %v the handler slept", d, handlerDelay)
	}
}

// TestWithQueryLogging_ErrorLogsDuration asserts a failed query is still
// logged at error level, now with the duration alongside it.
func TestWithQueryLogging_ErrorLogsDuration(t *testing.T) {
	logger, buf := newCapturingLogger()

	wantErr := errors.New("read model unavailable")
	handler := WithQueryLogging(logger, eventsourcing.NewQueryHandlerFunc(
		func(ctx context.Context, qry testQuery) (*testQueryResult, error) {
			return nil, wantErr
		},
	))

	if _, err := handler.HandleQuery(context.Background(), testQuery{QueryID: "q-2"}); !errors.Is(err, wantErr) {
		t.Fatalf("HandleQuery returned %v, want %v", err, wantErr)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}

	done := recs[1]
	if done["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", done["level"])
	}
	if done["msg"] != "Query failed" {
		t.Errorf("msg = %v, want %q", done["msg"], "Query failed")
	}
	if done["error"] != wantErr.Error() {
		t.Errorf("error = %v, want %q", done["error"], wantErr.Error())
	}
	if done["queryID"] != "q-2" {
		t.Errorf("queryID = %v, want %q", done["queryID"], "q-2")
	}
	requireDuration(t, done)
}

// TestQueryLogging_MiddlewareLogsCompletion asserts the bus-wide middleware
// form logs the same completion record as the wrapper it delegates to.
func TestQueryLogging_MiddlewareLogsCompletion(t *testing.T) {
	logger, buf := newCapturingLogger()

	middleware := QueryLogging(logger)
	gateway := middleware(func(ctx context.Context, qry eventsourcing.Query) (any, error) {
		return &testQueryResult{Value: 1}, nil
	})

	if _, err := gateway(context.Background(), testQuery{QueryID: "q-3"}); err != nil {
		t.Fatalf("gateway: %v", err)
	}

	recs := logRecords(t, buf)
	if len(recs) != 2 {
		t.Fatalf("logged %d records, want 2: %v", len(recs), recs)
	}
	if recs[1]["msg"] != "Query succeeded" {
		t.Errorf("completion msg = %v, want %q", recs[1]["msg"], "Query succeeded")
	}
	requireDuration(t, recs[1])
}
