package tasklist

import "context"

type ListTasks struct{}

func (q ListTasks) ID() []byte { return []byte("task-list") }

type QueryHandler struct {
	projector *Projector
}

func NewQueryHandler(p *Projector) *QueryHandler {
	return &QueryHandler{projector: p}
}

func (h *QueryHandler) HandleQuery(_ context.Context, _ ListTasks) ([]Task, error) {
	return h.projector.All(), nil
}
