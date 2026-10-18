package sqlite_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	cqrs "github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/projection"
	"github.com/terraskye/eventsourcing/sqlite"
)

// TestRunner_HandlerWritesAreExactlyOnce runs a projection whose read
// model is a table in the same database. The handler fails once halfway
// through a batch; the batch's writes roll back with it, so after the
// retry every event has produced exactly one row.
func TestRunner_HandlerWritesAreExactlyOnce(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			db := openDB(t, d.path(t))
			if _, err := db.ExecContext(t.Context(), `CREATE TABLE tasks (id TEXT NOT NULL, global_position INTEGER NOT NULL)`); err != nil {
				t.Fatalf("create read model: %v", err)
			}
			store := sqlite.NewEventStore(db)
			appendEvents(t, store, "task-1", &taskCreated{ID: "1"})
			appendEvents(t, store, "task-2", &taskCreated{ID: "2"})
			last := appendEvents(t, store, "task-3", &taskCreated{ID: "3"})

			var failed atomic.Bool
			handler := cqrs.NewEventHandlerFunc(func(ctx context.Context, e cqrs.Event) error {
				created := e.(*taskCreated)
				if _, err := sqlite.Tx(ctx).ExecContext(ctx,
					`INSERT INTO tasks (id, global_position) VALUES (?, ?)`,
					created.ID, cqrs.GlobalPositionFromContext(ctx),
				); err != nil {
					return err
				}
				if created.ID == "2" && failed.CompareAndSwap(false, true) {
					return errors.New("boom")
				}
				return nil
			})
			r := projection.NewRunner(projection.Definition{
				Name:         "tasks",
				Handler:      handler,
				EventTypes:   []string{"sqlite_test.TaskCreated"},
				PollInterval: 10 * time.Millisecond,
				ClaimTTL:     time.Second,
			}, store, sqlite.NewProjectionStore(db))

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				_ = r.Run(ctx)
				close(done)
			}()
			defer func() {
				cancel()
				<-done
			}()

			waitCtx, waitCancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer waitCancel()
			if err := r.WaitUntil(waitCtx, last); err != nil {
				t.Fatalf("WaitUntil: %v", err)
			}
			if !failed.Load() {
				t.Fatal("the handler never failed; the test did not exercise a rollback")
			}

			rows, err := db.QueryContext(t.Context(), `SELECT id, count(*) FROM tasks GROUP BY id ORDER BY id`)
			if err != nil {
				t.Fatalf("read model: %v", err)
			}
			defer rows.Close()
			got := map[string]int{}
			for rows.Next() {
				var (
					id string
					n  int
				)
				if err := rows.Scan(&id, &n); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got[id] = n
			}
			if len(got) != 3 || got["1"] != 1 || got["2"] != 1 || got["3"] != 1 {
				t.Fatalf("rows per task = %v, want exactly one for each of 1, 2 and 3", got)
			}
		})
	}
}
