//go:build integration

package kurrentdb

import (
	"regexp"
	"testing"

	kdb "github.com/kurrent-io/KurrentDB-Client-Go/kurrentdb"
)

// TestWithFilterEvents_EmptySliceMeansCatchAll is a regression test for
// GitHub issue #46: WithFilterEvents built the server-side filter regex as
// fmt.Sprintf("^(%s)$", strings.Join(filteredEvents, "|")) with no special
// case for an empty list, producing the literal regex "^()$" — which
// matches only the empty string, so a subscriber configured with a nil or
// empty filter never received a single event, silently. Every other
// EventBus implementation in this module treats an empty filter as "no
// filtering — deliver everything".
func TestWithFilterEvents_EmptySliceMeansCatchAll(t *testing.T) {
	opts := &kdb.PersistentAllSubscriptionOptions{}
	WithFilterEvents(nil)(opts)

	if opts.Filter == nil {
		// No filter at all is also a valid way to mean "catch-all" — the
		// fix takes this path, leaving opts.Filter unset.
		return
	}

	re, err := regexp.Compile(opts.Filter.Regex)
	if err != nil {
		t.Fatalf("built an invalid regex %q: %v", opts.Filter.Regex, err)
	}

	for _, eventType := range []string{"OrderCreated", "ItemAdded", "leakEvent"} {
		if !re.MatchString(eventType) {
			t.Errorf("empty filter should match every event type (catch-all, matching the "+
				"memory/file/postgres EventBus convention), but regex %q does not match %q",
				opts.Filter.Regex, eventType)
		}
	}
}
