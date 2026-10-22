package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/moby/moby/client"
	"github.com/smokinstack/sddb/internal/types"
)

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

func TestForgetGoneContainers(t *testing.T) {
	d := &DockerClient{prevStats: map[string]prevNetStats{"old": {}, "kept": {}}}
	d.forgetGone(map[string]bool{"kept": true, "new": true})
	if _, ok := d.prevStats["old"]; ok || len(d.prevStats) != 1 {
		t.Fatalf("prevStats = %v, want only the container still present", d.prevStats)
	}
}

// The CPU percentage needs two samples a second apart (PreCPUStats). The
// daemon only takes them when the stats request is stream=false WITHOUT
// one-shot=true; with one-shot, every container reads 0% CPU. This checks the
// request the agent actually sends, via a fake Engine API.
func TestStatsRequestAsksForPreviousSample(t *testing.T) {
	var statsQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		if strings.HasSuffix(r.URL.Path, "/stats") {
			statsQuery = r.URL.Query()
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	d := &DockerClient{cli: cli, api: cli, prevStats: map[string]prevNetStats{}}
	state := types.ContainerState{ID: "abc"}
	if err := d.fillStats(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	if statsQuery.Get("stream") != "false" || statsQuery.Has("one-shot") {
		t.Fatalf("stats query = %v; want stream=false and no one-shot (else CPU always reads 0%%)", statsQuery)
	}
}
