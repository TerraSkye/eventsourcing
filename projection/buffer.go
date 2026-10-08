package projection

import "context"

// Flusher collects the writes of one batch of events, to be written in bulk
// once the batch is complete; see [WithBuffer].
type Flusher interface {
	// Flush writes everything collected during the batch. The runner calls
	// it after the batch's last event, inside the batch's unit of work, so
	// ctx carries the same transaction the handlers see.
	Flush(ctx context.Context) error
}

// WithBuffer gives every batch a fresh buffer, created by newBuffer, that
// handlers collect writes in instead of writing them one by one. After the
// last event of a batch, the runner calls the buffer's Flush inside the
// batch's unit of work, before committing it.
//
// Handlers get the buffer with [BufferFrom]:
//
//	func (p *Projector) OnTaskCreated(ctx context.Context, e *events.TaskCreated) error {
//		b := projection.BufferFrom[*taskBuffer](ctx)
//		b.inserts[e.TaskID.String()] = &Task{ID: e.TaskID.String(), Title: e.Title}
//		return nil
//	}
//
//	taskList := projection.NewRunner("task-list", projector.EventHandlers(), store,
//		projection.WithCheckpoints(postgres.NewCheckpoints(pool)),
//		projection.WithBuffer(newTaskBuffer),
//	)
//
// The same handlers run while catching up and while live; only the size of
// the batches differs. While catching up, a batch of hundreds of events
// becomes a handful of bulk statements; while live, a batch usually holds
// one event, and Flush writes it right away.
//
// The buffer belongs to the batch, not to the projector. A batch that fails
// takes its buffer with it, so a retry never sees writes collected during
// the failed attempt. Handlers must still respect the order of events
// within a batch; for example, an event that updates a row inserted earlier
// in the same batch has to update the pending insert in the buffer.
func WithBuffer[B Flusher](newBuffer func() B) Option {
	return func(c *config) {
		c.newBuffer = func() Flusher { return newBuffer() }
	}
}

// BufferFrom returns the current batch's buffer, as created by the function
// passed to [WithBuffer].
//
// It panics if ctx carries no buffer, or one of a different type: both are
// wiring mistakes, a handler of a projection created without WithBuffer, or
// with a buffer of another type.
func BufferFrom[B Flusher](ctx context.Context) B {
	b, ok := ctx.Value(bufferKey).(B)
	if !ok {
		panic("projection: BufferFrom called without a matching WithBuffer")
	}
	return b
}
