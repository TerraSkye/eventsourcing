package taskstoarchive_test

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/automation"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/taskstoarchive"
	"github.com/terraskye/eventsourcing/slicetest"
)

// TestTasksToArchive covers the todo list of the archive automation: a
// task is claimable once its delay since completion has passed, and an
// archived task is not claimable at all. A claimed task is leased, so a
// second claim within the lease returns nothing.
func TestTasksToArchive(t *testing.T) {
	const delay = time.Hour
	id := uuid.New()
	now := time.Now()
	completedLongAgo := &events.TaskCompleted{TaskID: id, CompletedAt: now.Add(-2 * delay)}

	tests := []struct {
		name  string
		given []eventsourcing.Event
		want  []uuid.UUID
	}{
		{name: "nothing completed"},
		{
			name:  "due after its delay",
			given: []eventsourcing.Event{completedLongAgo},
			want:  []uuid.UUID{id},
		},
		{
			name:  "not due within its delay",
			given: []eventsourcing.Event{&events.TaskCompleted{TaskID: id, CompletedAt: now}},
		},
		{
			name:  "gone once archived",
			given: []eventsourcing.Event{completedLongAgo, &events.TaskArchived{TaskID: id}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			list := taskstoarchive.NewTasksToArchive(delay)
			slicetest.View(t, taskstoarchive.NewProjection(list)).
				Given(tt.given...).
				Then(func(t testing.TB) {
					req := automation.ClaimRequest{MaxPerKey: 10, Lease: time.Minute}
					got, err := list.Claim(t.Context(), req)
					if err != nil || !slices.Equal(got, tt.want) {
						t.Fatalf("Claim = %v, %v; want %v", got, err, tt.want)
					}
					if again, _ := list.Claim(t.Context(), req); len(again) != 0 {
						t.Fatalf("second Claim = %v, want nothing while the first lease runs", again)
					}
				})
		})
	}
}
