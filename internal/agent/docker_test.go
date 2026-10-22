package agent

import "testing"

func TestCounterRate(t *testing.T) {
	cases := []struct {
		name      string
		cur, prev uint64
		secs      float64
		want      float64
	}{
		{"normal", 15_000, 5_000, 5, 2_000},
		{"idle", 5_000, 5_000, 5, 0},
		{"counter reset after restart", 1_000, 9_000_000, 5, 0},
		{"no elapsed time", 10, 0, 0, 0},
	}
	for _, c := range cases {
		if got := counterRate(c.cur, c.prev, c.secs); got != c.want {
			t.Errorf("%s: counterRate(%d, %d, %v) = %v, want %v", c.name, c.cur, c.prev, c.secs, got, c.want)
		}
	}
}
