package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

// fakeAgent is a plain-HTTP stand-in for sddb-agent.
func fakeAgent(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/containers":
			json.NewEncoder(w).Encode(types.StatsResponse{Agent: types.AgentInfo{ID: "agent-1"}})
		case strings.HasSuffix(r.URL.Path, "/logs"):
			w.Write([]byte("log line\n"))
		default:
			json.NewEncoder(w).Encode(types.CommandResponse{Success: true, Message: "ok"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestDashboard(t *testing.T, agentAddr string) *Dashboard {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := NewState("")
	state.AddAgent(agentAddr, "")
	poller := NewPoller(state, time.Hour, nil, cfg)
	return &Dashboard{
		state:     state,
		poller:    poller,
		cfg:       cfg,
		agentHTTP: newAgentClient(nil),
		limiter:   newLoginLimiter(),
	}
}

func statsWith(id string, containers ...types.ContainerState) types.StatsResponse {
	return types.StatsResponse{Agent: types.AgentInfo{ID: id}, Containers: containers}
}

// Commands and log requests must not touch the agent map while the poller is
// re-keying it (agent IDs change whenever an agent restarts). Before the fix
// this was an unlocked map read racing a map write, which Go can turn into a
// fatal "concurrent map read and map write" crash of the whole dashboard.
// Run with -race.
func TestHandlersDoNotRaceWithPollUpdates(t *testing.T) {
	agent := fakeAgent(t)
	addr := strings.TrimPrefix(agent.URL, "http://")
	d := newTestDashboard(t, addr)

	ctr := types.ContainerState{ID: "abc123abc123def", ShortID: "abc123abc123", Name: "web", State: "running"}
	d.state.UpdateFromPoll(addr, statsWith("id-0", ctr))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Changing ID forces UpdateFromPoll to delete and re-insert the map entry.
			d.state.UpdateFromPoll(addr, statsWith(fmt.Sprintf("id-%d", i), ctr))
		}
	}()

	for i := 0; i < 200; i++ {
		body, _ := json.Marshal(map[string]string{"agent_addr": addr, "container_id": "web", "action": "restart"})
		rec := httptest.NewRecorder()
		d.handleCommand(rec, httptest.NewRequest(http.MethodPost, "/api/command", bytes.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("command: status %d: %s", rec.Code, rec.Body.String())
		}

		rec = httptest.NewRecorder()
		d.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?agent="+addr+"&container=web", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("logs: status %d: %s", rec.Code, rec.Body.String())
		}
	}
	close(stop)
	wg.Wait()
}

func TestStateGetReturnsIndependentCopy(t *testing.T) {
	s := NewState("")
	s.AddAgent("h:1", "")
	s.UpdateFromPoll("h:1", statsWith("a", types.ContainerState{Name: "one"}))

	got, ok := s.Get("h:1")
	if !ok || len(got.LastStats.Containers) != 1 {
		t.Fatalf("Get = %+v, %v", got, ok)
	}
	got.LastStats.Containers[0].Name = "mutated"

	again, _ := s.Get("h:1")
	if again.LastStats.Containers[0].Name != "one" {
		t.Fatal("Get must return a copy; caller mutation leaked into state")
	}
	if _, ok := s.Get("nope:1"); ok {
		t.Fatal("unknown address should not be found")
	}
}

func TestLogsRequestIsValidatedAndBounded(t *testing.T) {
	var gotPath, gotTail string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotTail = r.URL.EscapedPath(), r.URL.Query().Get("tail")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	d := newTestDashboard(t, addr)
	d.state.UpdateFromPoll(addr, statsWith("a", types.ContainerState{ID: "abc123full", ShortID: "abc123", Name: "web"}))

	get := func(query string) int {
		rec := httptest.NewRecorder()
		d.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?agent="+addr+"&"+query, nil))
		return rec.Code
	}

	if code := get("container=web&tail=all"); code != 200 || gotTail != "100" {
		t.Errorf("tail=all: status %d, agent got tail=%q; want 200 and the default 100", code, gotTail)
	}
	if get("container=web&tail=999999"); gotTail != "5000" {
		t.Errorf("huge tail reached the agent as %q, want capped at 5000", gotTail)
	}
	if get("container=web&tail=1%26x%3D1"); gotTail != "100" {
		t.Errorf("tail with injected query reached the agent as %q", gotTail)
	}
	if get("container=web"); gotPath != "/api/containers/abc123full/logs" {
		t.Errorf("agent path = %q, want the agent-reported ID", gotPath)
	}
	if code := get("container=../info%3Fx%3D"); code != http.StatusBadRequest {
		t.Errorf("unknown/path-like container: status %d, want 400", code)
	}
}
