package tasklist

import (
	"context"
	"sync"

	"github.com/terraskye/eventsourcing"
	"github.com/terraskye/eventsourcing/examples/todolist/events"
)

// Task is the cached read model entry.
type Task struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

// Projector maintains the cached task list, kept up to date via the event bus.
type Projector struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewProjector() *Projector {
	return &Projector{tasks: make(map[string]*Task)}
}

// All returns a snapshot of the current task list.
func (p *Projector) All() []Task {
	p.mu.RLock()
	defer p.mu.RUnlock()

	result := make([]Task, 0, len(p.tasks))
	for _, t := range p.tasks {
		result = append(result, *t)
	}
	return result
}

// OnTaskCreated is called by the event bus when a TaskCreated event arrives.
func (p *Projector) OnTaskCreated(_ context.Context, e *events.TaskCreated) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tasks[e.TaskID.String()] = &Task{
		ID:    e.TaskID.String(),
		Title: e.Title,
	}
	return nil
}

// OnTaskCompleted is called by the event bus when a TaskCompleted event arrives.
func (p *Projector) OnTaskCompleted(_ context.Context, e *events.TaskCompleted) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.tasks[e.TaskID.String()]; ok {
		t.Completed = true
	}
	return nil
}

// TaskArchived is called by the event bus when a TaskCompleted event arrives.
func (p *Projector) OnTaskArchived(_ context.Context, e *events.TaskArchived) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	delete(p.tasks, e.TaskID.String())
	return nil
}

// EventHandlers returns the typed handlers to register on the event bus.
func (p *Projector) EventHandlers() *eventsourcing.EventGroupProcessor {
	return eventsourcing.NewEventGroupProcessor(
		eventsourcing.OnEvent(p.OnTaskCreated),
		eventsourcing.OnEvent(p.OnTaskCompleted),
		eventsourcing.OnEvent(p.OnTaskArchived),
	)
}
