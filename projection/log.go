package projection

import (
	"context"
	"log/slog"
	"time"
)

// log writes a lifecycle record for seg to the projection's Logger, with
// the attributes every record carries: projection, segment, owner,
// position and head. position is passed in because the in-memory status
// may lag behind it, as right after a claim.
func (r *Runner) log(ctx context.Context, level slog.Level, seg int, position uint64, msg string, extra ...any) {
	if !r.p.Logger.Enabled(ctx, level) {
		return
	}
	r.mu.Lock()
	head := r.segs[seg].status.Head
	r.mu.Unlock()
	args := append([]any{
		slog.String("projection", r.p.Name),
		slog.Int("segment", seg),
		slog.String("owner", r.p.Owner),
		slog.Uint64("position", position),
		slog.Uint64("head", head),
	}, extra...)
	r.p.Logger.Log(ctx, level, msg, args...)
}

// position returns seg's position as this instance last recorded it.
func (r *Runner) position(seg int) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.segs[seg].status.Position
}

// logStall records that seg stalled on err and retries after wait.
func (r *Runner) logStall(ctx context.Context, seg int, err error, wait time.Duration) {
	r.log(ctx, slog.LevelError, seg, r.position(seg), "projection segment stalled",
		slog.String("error", err.Error()), slog.Duration("retry_in", wait))
}
