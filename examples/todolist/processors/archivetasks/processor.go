package archivetasks

import (
	"context"
	"log"
	"time"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
	"github.com/terraskye/eventsourcing/examples/todolist/slices/archivetask"
)

// Processor reacts to TaskCompleted events by scheduling an ArchiveTask
// command after a configurable delay — a saga/policy that closes the loop
// between events and new commands.
type Processor struct {
	bus   eventsourcing.Dispatcher
	delay time.Duration // configurable — use 30*24*time.Hour in production
}

func NewProcessor(bus eventsourcing.Dispatcher, delay time.Duration) *Processor {
	return &Processor{delay: delay, bus: bus}
}

// OnTaskCompleted is called when a task is completed.
// It schedules the archive command after the configured delay.
func (p *Processor) OnTaskCompleted(ctx context.Context, e *events.TaskCompleted) error {
	go func() {
		select {
		case <-time.After(p.delay):
			cmd := archivetask.ArchiveTask{TaskID: e.TaskID}

			if _, err := p.bus.Dispatch(context.Background(), cmd); err != nil {
				log.Printf("archive task %s: %v", e.TaskID, err)
			}
		case <-ctx.Done():
			// Subscription cancelled; skip
		}
	}()

	return nil
}

// EventHandlers returns the event group processor to register on the bus.
func (p *Processor) EventHandlers() *eventsourcing.EventGroupProcessor {
	return eventsourcing.NewEventGroupProcessor(
		eventsourcing.OnEvent(p.OnTaskCompleted),
	)
}
