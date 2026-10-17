package sqlite

import (
	"context"
	"fmt"

	cqrs "github.com/terraskye/eventsourcing"
)

// Import copies every event src holds into dst, in global order, keeping
// each event's ID, stream, order within its stream, metadata and time. It
// returns the number of events copied.
//
// It moves a log from another store, such as a file.EventStore, to SQLite.
// Run it from the program that defines the events, after registering them
// with [cqrs.RegisterEvent], since reading src decodes every event. dst
// should be empty: an event whose stream position is already taken in dst
// fails the import with a [cqrs.StreamRevisionConflictError], and the
// events copied before it stay.
//
// Global positions are assigned anew by dst, so they match src's only when
// src's positions start at 1 without gaps.
func Import(ctx context.Context, src cqrs.GlobalReader, dst *EventStore) (int, error) {
	var (
		after  uint64
		copied int
		// counts holds how many events of each stream are copied so far.
		// Positions come from the global order rather than the source's
		// Version, which not every store fills in.
		counts = map[string]uint64{}
	)
	for {
		page, err := src.ReadAll(ctx, after, cqrs.ReadOptions{Limit: defaultReadLimit})
		if err != nil {
			return copied, fmt.Errorf("sqlite: import: read after %d: %w", after, err)
		}
		for _, env := range page.Events {
			// Requiring the stream's copied length as revision keeps positions
			// identical and catches a dst that already holds the stream.
			n := counts[env.StreamID]
			if _, err := dst.Save(ctx, []cqrs.Envelope{*env}, cqrs.Revision(n)); err != nil {
				return copied, fmt.Errorf("sqlite: import: event %d of stream %q: %w", n+1, env.StreamID, err)
			}
			counts[env.StreamID] = n + 1
			copied++
		}
		after = page.Next
		if page.AtEnd {
			return copied, nil
		}
	}
}
