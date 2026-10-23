package projection

import "testing"

func TestQuota(t *testing.T) {
	m := func(owner string, version, segments int) Instance {
		return Instance{Owner: owner, Version: version, Segments: segments}
	}
	tests := []struct {
		name      string
		instances []Instance
		segments  int
		want      map[string]int // every instance's quota, unless partial
		partial   bool
	}{
		{
			name:      "alone",
			instances: []Instance{m("a", 0, 0)},
			segments:  8,
			want:      map[string]int{"a": 8},
		},
		{
			name:      "even",
			instances: []Instance{m("a", 0, 8), m("b", 0, 0)},
			segments:  8,
			want:      map[string]int{"a": 4, "b": 4},
		},
		{
			name:      "rest to the owners first on a tie",
			instances: []Instance{m("a", 0, 0), m("b", 0, 0), m("c", 0, 0)},
			segments:  4,
			want:      map[string]int{"a": 2, "b": 1, "c": 1},
		},
		{
			name:      "rest to the instances holding most",
			instances: []Instance{m("a", 0, 0), m("b", 0, 2), m("c", 0, 2)},
			segments:  4,
			want:      map[string]int{"a": 1, "b": 2, "c": 1},
		},
		{
			name:      "single segment stays with its holder",
			instances: []Instance{m("a", 0, 0), m("b", 0, 1)},
			segments:  1,
			want:      map[string]int{"a": 0, "b": 1},
		},
		{
			name:      "more instances than segments",
			instances: []Instance{m("a", 0, 0), m("b", 0, 0), m("c", 0, 0)},
			segments:  2,
			want:      map[string]int{"a": 1, "b": 1, "c": 0},
		},
		{
			name:      "older version counts for nothing",
			instances: []Instance{m("a", 1, 4), m("b", 2, 0)},
			segments:  4,
			want:      map[string]int{"a": 0, "b": 4},
		},
		{
			name:      "not live",
			instances: []Instance{m("a", 0, 0)},
			segments:  4,
			want:      map[string]int{"z": 0},
			partial:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sum := 0
			for owner, want := range tt.want {
				got := quota(tt.instances, owner, tt.segments)
				if got != want {
					t.Errorf("quota(%s) = %d, want %d", owner, got, want)
				}
				sum += got
			}
			if !tt.partial && sum != tt.segments {
				t.Errorf("quotas sum to %d, want %d", sum, tt.segments)
			}
		})
	}
}
