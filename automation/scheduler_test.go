package automation

import (
	"context"
	"strconv"
	"testing"

	"github.com/terraskye/eventsourcing/projection"
)

func TestShardKey_MatchesProjectionSegmentOf(t *testing.T) {
	for _, id := range []string{"a", "order-1", "f2d4"} {
		if got, want := ShardKey(id, 16), strconv.Itoa(projection.SegmentOf(id, 16)); got != want {
			t.Fatalf("ShardKey(%q) = %s, projection says %s", id, got, want)
		}
	}
	if got := Shards(3); got == nil {
		t.Fatal("Shards(3) is nil")
	}
	segs, _ := Shards(3).segments(context.Background())
	if len(segs) != 3 || segs[0].Key != "0" || segs[2].Key != "2" {
		t.Fatalf("Shards(3) = %+v", segs)
	}
}
