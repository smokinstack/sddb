package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

// upgradeAgent counts upgrade requests and answers like a current agent.
func upgradeAgent(t *testing.T, accept bool) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var upgrades atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/upgrade") {
			upgrades.Add(1)
			if !accept {
				http.Error(w, "compose file not found", http.StatusInternalServerError)
				return
			}
			json.NewEncoder(w).Encode(types.CommandResponse{Success: accept, Pending: accept, Message: "upgrade started"})
			return
		}
		json.NewEncoder(w).Encode(types.StatsResponse{})
	}))
	t.Cleanup(srv.Close)
	return srv, &upgrades
}

func autoPoller(t *testing.T, addr string) (*Poller, *time.Time) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update(func(c *config.Config) { c.AutoUpdate[addr+"::web"] = true })
	state := NewState("")
	state.AddAgent(addr, "")
	p := NewPoller(state, time.Hour, make(chan struct{}, 16), nil, cfg)
	now := time.Unix(1_700_000_000, 0)
	p.now = func() time.Time { return now }
	return p, &now
}

var webWithUpdate = []types.ContainerState{{ID: "c1", Name: "web", State: "running", UpdateAvailable: true}}

func TestAutoUpdateBacksOffAfterFailedJobs(t *testing.T) {
	agent, upgrades := upgradeAgent(t, true)
	addr := strings.TrimPrefix(agent.URL, "http://")
	p, now := autoPoller(t, addr)

	p.runAutoUpdates(addr, webWithUpdate, nil)
	if upgrades.Load() != 1 {
		t.Fatalf("first poll: %d upgrades, want 1", upgrades.Load())
	}

	// While the job runs nothing new is sent, however many polls happen.
	running := map[string]types.JobStatus{"web": {Action: "upgrade", State: types.JobRunning}}
	*now = now.Add(time.Hour)
	p.runAutoUpdates(addr, webWithUpdate, running)
	if upgrades.Load() != 1 {
		t.Fatalf("upgrade re-sent while job running: %d", upgrades.Load())
	}

	// The job fails: wait 10 minutes before trying again …
	failed := func(at time.Time) map[string]types.JobStatus {
		return map[string]types.JobStatus{"web": {Action: "upgrade", State: types.JobFailed, Error: "compose pull: exit 1", Finished: at.Unix()}}
	}
	jobs := failed(*now)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	*now = now.Add(9 * time.Minute)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	if upgrades.Load() != 1 {
		t.Fatalf("retried within the first backoff: %d upgrades", upgrades.Load())
	}
	*now = now.Add(2 * time.Minute)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	if upgrades.Load() != 2 {
		t.Fatalf("no retry after the first backoff: %d upgrades", upgrades.Load())
	}

	// … and 20 minutes after the second failure (doubling).
	jobs = failed(*now)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	*now = now.Add(19 * time.Minute)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	if upgrades.Load() != 2 {
		t.Fatalf("retried within the doubled backoff: %d upgrades", upgrades.Load())
	}
	*now = now.Add(2 * time.Minute)
	p.runAutoUpdates(addr, webWithUpdate, jobs)
	if upgrades.Load() != 3 {
		t.Fatalf("no retry after the doubled backoff: %d upgrades", upgrades.Load())
	}
}

func TestAutoUpdateBacksOffWhenAgentRefuses(t *testing.T) {
	agent, upgrades := upgradeAgent(t, false)
	addr := strings.TrimPrefix(agent.URL, "http://")
	p, now := autoPoller(t, addr)

	for i := 0; i < 20; i++ { // 20 polls, 5s apart
		p.runAutoUpdates(addr, webWithUpdate, nil)
		*now = now.Add(5 * time.Second)
	}
	if upgrades.Load() != 1 {
		t.Fatalf("refused upgrade retried every poll: %d requests in 100s", upgrades.Load())
	}
}

func TestFailureBackoff(t *testing.T) {
	cases := []struct {
		n    int
		err  string
		want time.Duration
	}{
		{1, "", 10 * time.Minute},
		{2, "", 20 * time.Minute},
		{3, "", 40 * time.Minute},
		{10, "", maxAutoBackoff},
		{100, "", maxAutoBackoff},
		{1, "toomanyrequests: rate limit", time.Hour},
	}
	for _, c := range cases {
		if got := failureBackoff(c.n, c.err); got != c.want {
			t.Errorf("failureBackoff(%d, %q) = %s, want %s", c.n, c.err, got, c.want)
		}
	}
}
