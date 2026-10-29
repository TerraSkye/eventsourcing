package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// TestWalkthrough runs the README's walkthrough against the wired service:
// create, list, complete, complete again, and the archive that follows.
func TestWalkthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// The completed task must still be listed right after it completes, so
	// the archive delay has to outlast completing and listing it.
	const archiveAfter = 500 * time.Millisecond
	a := newApp(slog.New(slog.NewTextHandler(io.Discard, nil)), archiveAfter, 50*time.Millisecond)
	defer a.close()

	ctx := t.Context()
	errc := make(chan error, 1)
	go func() { errc <- a.run(ctx) }()

	srv := httptest.NewServer(a.routes)
	defer srv.Close()
	tasks := srv.URL + "/api/v1/tasks"

	// Create a task. The response waits for the task list, so the list
	// right after it has the task: no sleep, no retry.
	resp := post(t, tasks, `{"title": "Buy groceries"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: got status %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	var created struct {
		TaskID string `json:"task_id"`
	}
	decode(t, resp, &created)

	if got := list(t, tasks); len(got) != 1 || got[0].ID != created.TaskID || got[0].Completed {
		t.Fatalf("list after create: got %+v, want one open task %s", got, created.TaskID)
	}

	// Complete it; again the list reflects it at once. The command nudges
	// the runners, so this takes milliseconds, not a poll interval: were it
	// to take the archive delay, the task could be archived before it is
	// listed.
	completing := time.Now()
	if resp := post(t, tasks+"/"+created.TaskID+"/complete", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("complete: got status %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	got := list(t, tasks)
	if took := time.Since(completing); took >= archiveAfter {
		t.Fatalf("complete and list took %v, want less than the archive delay %v", took, archiveAfter)
	}
	if len(got) != 1 || !got[0].Completed {
		t.Fatalf("list after complete: got %+v, want the task completed", got)
	}

	// Business rules and input validation answer 400.
	if resp := post(t, tasks+"/"+created.TaskID+"/complete", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("complete again: got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if resp := post(t, tasks+"/not-a-uuid/complete", ""); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("complete invalid ID: got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if resp := post(t, tasks, `{}`); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("create without title: got status %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	// The automation archives the completed task once it is due, and the
	// archived task leaves the list.
	deadline := time.Now().Add(5 * time.Second)
	for len(list(t, tasks)) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("task %s still listed 5s after completion, want it archived", created.TaskID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case err := <-errc:
		t.Fatalf("background processes stopped early: %v", err)
	default:
	}
}

type task struct {
	ID        string `json:"id"`
	Completed bool   `json:"completed"`
}

func list(t *testing.T, url string) []task {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: got status %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
	var body struct {
		Tasks []task `json:"tasks"`
	}
	decode(t, resp, &body)
	return body.Tasks
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}
