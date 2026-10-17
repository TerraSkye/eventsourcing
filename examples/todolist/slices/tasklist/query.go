package tasklist

import "context"

type ListTasks struct{}

func (q ListTasks) ID() []byte { return []byte("task-list") }

type QueryHandler struct {
	list *TaskList
}

func NewQueryHandler(list *TaskList) *QueryHandler {
	return &QueryHandler{list: list}
}

func (h *QueryHandler) HandleQuery(_ context.Context, _ ListTasks) ([]Task, error) {
	h.list.mu.RLock()
	defer h.list.mu.RUnlock()

	result := make([]Task, 0, len(h.list.tasks))
	for _, t := range h.list.tasks {
		result = append(result, *t)
	}
	return result, nil
}
