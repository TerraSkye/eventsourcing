package sqlite_test

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/automation/automationtest"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/sqlite"
)

// tasksToFinish is a todo list in SQLite: one row per task created and not
// yet done, with the end of its lease in Unix milliseconds.
type tasksToFinish struct {
	db *sql.DB
}

const tasksToFinishTable = `CREATE TABLE tasks_to_finish (
	task_id       TEXT    PRIMARY KEY,
	claimed_until INTEGER NOT NULL DEFAULT 0
)`

// Claim leases up to max pending tasks. SQLite runs one writer at a time,
// so the UPDATE needs no SKIP LOCKED to keep two claims apart.
func (l *tasksToFinish) Claim(ctx context.Context, max int, lease time.Duration) ([]string, error) {
	now := time.Now().UnixMilli()
	rows, err := l.db.QueryContext(ctx, `
		UPDATE tasks_to_finish SET claimed_until = ?
		WHERE task_id IN (
			SELECT task_id FROM tasks_to_finish
			WHERE claimed_until <= ?
			ORDER BY task_id
			LIMIT ?)
		RETURNING task_id`, now+lease.Milliseconds(), now, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, rows.Err()
}

// tasksToFinishAdapter drives tasksToFinish for the acceptance suite,
// writing the rows the projection would write.
type tasksToFinishAdapter struct {
	*tasksToFinish
}

func (a tasksToFinishAdapter) Add(ctx context.Context, id string) error {
	_, err := a.db.ExecContext(ctx, `INSERT INTO tasks_to_finish (task_id) VALUES (?)`, id)
	return err
}

func (a tasksToFinishAdapter) Complete(ctx context.Context, id string) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM tasks_to_finish WHERE task_id = ?`, id)
	return err
}

func TestTasksToFinish_ClaimAcceptance(t *testing.T) {
	automationtest.ClaimAcceptanceTest(t, func(t *testing.T) automationtest.TodoList {
		db := openDB(t, ":memory:")
		if _, err := db.ExecContext(t.Context(), tasksToFinishTable); err != nil {
			t.Fatalf("create todo list: %v", err)
		}
		return tasksToFinishAdapter{&tasksToFinish{db: db}}
	})
}

// TestAutomation_ClosesTheLoop runs the Event Modeling loop on one SQLite
// database: TaskCreated puts a task on the todo list, the automation claims
// it and records TaskDone, and the projection takes it off the list again.
func TestAutomation_ClosesTheLoop(t *testing.T) {
	db := openDB(t, ":memory:")
	if _, err := db.ExecContext(t.Context(), tasksToFinishTable); err != nil {
		t.Fatalf("create todo list: %v", err)
	}
	store := sqlite.NewEventStore(db)
	quiet := slog.New(slog.DiscardHandler)

	todoList := projection.NewRunner(projection.Definition{
		Name: "tasks-to-finish",
		Handler: eventsourcing.NewEventHandlerFunc(func(ctx context.Context, e eventsourcing.Event) error {
			var err error
			switch e := e.(type) {
			case *taskCreated:
				_, err = sqlite.Tx(ctx).ExecContext(ctx, `INSERT INTO tasks_to_finish (task_id) VALUES (?) ON CONFLICT DO NOTHING`, e.ID)
			case *taskDone:
				_, err = sqlite.Tx(ctx).ExecContext(ctx, `DELETE FROM tasks_to_finish WHERE task_id = ?`, e.ID)
			}
			return err
		}),
		EventTypes:   []string{"sqlite_test.TaskCreated", "sqlite_test.TaskDone"},
		PollInterval: 10 * time.Millisecond,
		ClaimTTL:     time.Second,
		Logger:       quiet,
	}, store, sqlite.NewProjectionStore(db))

	list := &tasksToFinish{db: db}
	finish := automation.New(automation.Definition[string]{
		Name:     "finish-tasks",
		TodoList: todoList,
		Claim:    list.Claim,
		Work: func(ctx context.Context, id string) error {
			// The command's outcome: the event that removes the item.
			_, err := store.Save(ctx, []eventsourcing.Envelope{{StreamID: "task-" + id, Event: &taskDone{ID: id}}}, eventsourcing.StreamExists{})
			return err
		},
		Interval: time.Hour, // nudges from the runner do the waking
		Logger:   quiet,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = todoList.Run(ctx) }()
	go func() { _ = finish.Run(ctx) }()

	for _, id := range []string{"1", "2", "3"} {
		appendEvents(t, store, "task-"+id, &taskCreated{ID: id})
	}
	todoList.Nudge()

	waitFor(t, "three tasks finished", func() bool { return finish.Status().Processed == 3 })
	waitFor(t, "an empty todo list", func() bool {
		var n int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM tasks_to_finish`).Scan(&n); err != nil {
			t.Fatalf("count todo list: %v", err)
		}
		return n == 0
	})
	res, err := store.ReadAll(t.Context(), 0, eventsourcing.ReadOptions{EventTypes: []string{"sqlite_test.TaskDone"}})
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("log holds %d TaskDone events, want 3: each task finished exactly once", len(res.Events))
	}
}
