package projection

import "context"

type ctxKey int

const (
	replayingKey ctxKey = iota
	bufferKey
)

// IsReplaying reports whether the event being handled under ctx is being
// processed again: the projection had already processed it before its last
// rebuild, or it was already in the log when the projection first started.
//
// Use it to skip work that only makes sense for new events, such as pushing
// a change to connected browsers. It is unrelated to catching up: a
// projection that falls behind and catches up processes new events, and
// IsReplaying reports false for them.
//
// IsReplaying reports false when ctx was not passed to a handler by a
// [Runner].
func IsReplaying(ctx context.Context) bool {
	v, _ := ctx.Value(replayingKey).(bool)
	return v
}
