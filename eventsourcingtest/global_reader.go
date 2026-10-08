package eventsourcingtest

import (
	"testing"

	"github.com/terraskye/eventsourcing"
)

// GlobalReaderAcceptanceTest runs the [eventsourcing.GlobalReader] contract
// against reader as a set of subtests. store must be the store reader reads
// from; the suite appends to it through Save and checks what ReadAll and
// Head return.
//
// Like [AcceptanceTest], it may run against a store that already holds
// events: it only looks at events in streams it created, named with a
// fresh UUID, and reads after the head it found when it started.
//
// The suite checks that:
//
//   - events across several streams come back in strictly ascending
//     GlobalVersion order, with no event returned twice;
//   - ReadAll(after = n) never returns the event at position n, and
//     after = 0 starts at the beginning of the log;
//   - Limit bounds the page, and paging with Next visits every event
//     exactly once;
//   - EventTypes and StreamPrefixes filter exactly, alone and combined,
//     and Next still moves past events that were filtered out;
//   - AtEnd is reported at the head, and when AtEnd is false, Next is
//     greater than after;
//   - Head never exceeds what ReadAll returns;
//   - under many concurrent Saves to different streams, some of them
//     multi-event, a reader paging along at the same time sees every event
//     exactly once, so no position is skipped because it became visible
//     late;
//   - if reader implements [eventsourcing.Notifier], a Save is followed by
//     a notification, and the channel is closed when ctx is done.
//
// The concurrency check is the one that catches stores whose positions can
// become visible out of order, such as SQL sequences assigned before
// commit; run it against the real database, not a mock.
func GlobalReaderAcceptanceTest(t *testing.T, store eventsourcing.EventStore, reader eventsourcing.GlobalReader) {
	t.Helper()
	t.Skip("eventsourcingtest: GlobalReaderAcceptanceTest is not implemented yet")
}
