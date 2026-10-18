package tasklist

import "sync"

// Task is one entry in the read model.
type Task struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Completed bool   `json:"completed"`
}

// TaskList is the read model: written by the projection, read by the query.
type TaskList struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

func NewTaskList() *TaskList {
	return &TaskList{tasks: make(map[string]*Task)}
}
