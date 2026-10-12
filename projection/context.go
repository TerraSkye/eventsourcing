package projection

import (
	"context"

	"github.com/terraskye/eventsourcing"
)

type ctxKey int

const (
	envelopeKey ctxKey = iota
	replayUntilKey
)

// withEnvelope attaches an envelope and the segment's ReplayUntil to ctx,
// so handlers can call [IsReplaying] and the usual [eventsourcing]
// *FromContext accessors.
func withEnvelope(ctx context.Context, env *eventsourcing.Envelope, replayUntil uint64) context.Context {
	ctx = context.WithValue(ctx, envelopeKey, env)
	ctx = context.WithValue(ctx, replayUntilKey, replayUntil)
	// Thread the envelope through the standard eventsourcing accessors too,
	// so GlobalVersionFromContext etc. keep working.
	return eventsourcing.WithEnvelope(ctx, env)
}

// IsReplaying reports whether the event being handled under ctx is being
// processed again: either it was already in the log when the projection
// first started, or the projection has been rebuilt since it last saw it.
//
// Use it to skip work that only makes sense for new events, such as
// pushing a change to connected browsers. It is unrelated to catching up:
// a projection that falls behind and then catches up is processing new
// events, and IsReplaying reports false for them.
//
// IsReplaying reports false when ctx was not passed to a handler by a
// [Runner].
func IsReplaying(ctx context.Context) bool {
	env, _ := ctx.Value(envelopeKey).(*eventsourcing.Envelope)
	if env == nil {
		return false
	}
	ru, _ := ctx.Value(replayUntilKey).(uint64)
	return env.GlobalVersion <= ru
}
