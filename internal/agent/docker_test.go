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

func TestDiskUsageMatchesDf(t *testing.T) {
	// udocker last night: df showed 98G size, 89G used, 4.4G avail, 96%.
	const gib = 1 << 30
	blocks, free, avail := uint64(98*gib/4096), uint64(9*gib/4096), uint64(44*gib/10/4096)
	used, total := diskUsage(blocks, free, avail, 4096)
	pct := float64(used) / float64(total) * 100
	if pct < 95 || pct > 96 {
		t.Fatalf("disk = %.1f%%, want ~95.3%% like df (old formula gave %.1f%%)", pct, float64(blocks-free)/float64(blocks)*100)
	}
}
