package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"github.com/smokinstack/sddb/internal/types"
)

// Config holds agent runtime configuration.
type Config struct {
	ID             string
	ListenAddr     string
	UpdateInterval time.Duration
	Version        string      // reported to the dashboard
	TLS            *tls.Config // nil = plain HTTP
}

// Agent polls Docker and serves an HTTP API.
type Agent struct {
	cfg    Config
	docker *DockerClient
	host   *hostSampler

	mu         sync.RWMutex
	lastStats  types.StatsResponse
	lastUpdate time.Time

	// Per-image update check cache: imageID → (available, checkedAt)
	updateCache   map[string]updateEntry
	updateCacheMu sync.Mutex

	// Background upgrades and pulls, keyed by container name.
	jobs      *jobTracker
	onJobDone func() // runs after each job; refreshes stats (overridable in tests)

	// Registry update checks (see updates.go). Function fields so tests can
	// replace Docker and the registry.
	kickUpdates   chan struct{}
	now           func() time.Time
	repoDigests   func(ctx context.Context, imageID string) ([]string, error)
	checkRegistry func(ctx context.Context, imageName string, repoDigests []string) (bool, error)
}

type updateEntry struct {
	available bool
	checkedAt time.Time
}

func New(cfg Config, docker *DockerClient) *Agent {
	a := &Agent{
		cfg:         cfg,
		docker:      docker,
		host:        &hostSampler{},
		updateCache: make(map[string]updateEntry),
		jobs:        newJobTracker(),
		kickUpdates: make(chan struct{}, 1),
		now:         time.Now,
	}
	a.repoDigests = a.dockerRepoDigests
	a.checkRegistry = CheckUpdateAvailable
	a.onJobDone = func() {
		a.refresh(context.Background())
		a.kickUpdateCheck() // the upgraded container's new image hasn't been checked yet
	}
	return a
}

// Run starts the background refresh loop and the HTTP server.
// It blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// initial collection
	if err := a.refresh(ctx); err != nil {
		log.Printf("initial stats collection failed: %v", err)
	}

	go a.refreshLoop(ctx)
	go a.updateCheckLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", a.handleInfo)
	mux.HandleFunc("/api/containers", a.handleContainers)
	mux.HandleFunc("/api/containers/", a.handleCommand) // /{id}/start|stop|restart|upgrade

	srv := &http.Server{
		Addr:      a.cfg.ListenAddr,
		Handler:   mux,
		TLSConfig: a.cfg.TLS,
		// Bound slow/idle clients so stale connections can't pile up.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	if a.cfg.TLS != nil {
		log.Printf("sddb-agent %s (id %s) listening on %s (TLS, interval %s)", a.cfg.Version, a.cfg.ID, a.cfg.ListenAddr, a.cfg.UpdateInterval)
		// Cert and key are already embedded in TLSConfig via tls.X509KeyPair
		if err := srv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			return err
		}
	} else {
		log.Printf("sddb-agent %s (id %s) listening on %s (interval %s)", a.cfg.Version, a.cfg.ID, a.cfg.ListenAddr, a.cfg.UpdateInterval)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
	}
	return nil
}

func (a *Agent) refreshLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.UpdateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.refresh(ctx); err != nil {
				log.Printf("stats refresh error: %v", err)
			}
		}
	}
}

func (a *Agent) refresh(ctx context.Context) error {
	containers, err := a.docker.CollectAll(ctx)
	if err != nil {
		return err
	}

	// Update status comes from the background checker's cache; refresh never
	// waits on a registry.
	for i := range containers {
		containers[i].UpdateAvailable = a.updateAvailable(containers[i].ImageID)
	}

	hostname := getHostname()
	resp := types.StatsResponse{
		Agent: types.AgentInfo{
			ID:             a.cfg.ID,
			Hostname:       hostname,
			UpdateInterval: int(a.cfg.UpdateInterval.Seconds()),
			Version:        a.cfg.Version,
		},
		Host:       a.host.sample(containers),
		Containers: containers,
		Timestamp:  time.Now().Unix(),
		Jobs:       a.jobs.snapshot(),
	}

	a.mu.Lock()
	a.lastStats = resp
	a.lastUpdate = time.Now()
	a.mu.Unlock()
	return nil
}

// handleInfo returns static agent information.
func (a *Agent) handleInfo(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	info := a.lastStats.Agent
	a.mu.RUnlock()
	writeJSON(w, info)
}

// handleContainers returns the full stats snapshot.
func (a *Agent) handleContainers(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	resp := a.lastStats
	a.mu.RUnlock()
	writeJSON(w, resp)
}

// handleLogs serves GET /api/containers/{id}/logs?tail=N
func (a *Agent) handleLogs(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/containers/"), "/")
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	containerID := parts[0]
	tail := strconv.Itoa(types.LogTail(r.URL.Query().Get("tail")))

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	logs, err := a.docker.GetLogs(ctx, containerID, tail)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(logs))
}

// handleCommand processes POST /api/containers/{id}/{action}.
func (a *Agent) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.handleLogs(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// path: /api/containers/{id}/{action}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/containers/"), "/")
	if len(parts) != 2 {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	containerID, action := parts[0], parts[1]

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var err error
	switch action {
	case "start":
		err = a.docker.StartContainer(ctx, containerID)
	case "stop":
		err = a.docker.StopContainer(ctx, containerID)
	case "restart":
		err = a.docker.RestartContainer(ctx, containerID)
	case "upgrade", "pull":
		// Long-running: start in the background and report via stats Jobs.
		a.startJob(ctx, w, containerID, action)
		return
	default:
		http.Error(w, fmt.Sprintf("unknown action: %s", action), http.StatusBadRequest)
		return
	}

	if err != nil {
		writeJSON(w, types.CommandResponse{Success: false, Message: err.Error()})
		return
	}

	// Trigger immediate stats refresh
	go func() {
		time.Sleep(500 * time.Millisecond)
		a.refresh(context.Background())
	}()

	writeJSON(w, types.CommandResponse{Success: true, Message: "ok"})
}

// startJob starts an upgrade or pull of containerID in the background and
// responds immediately. The outcome appears in StatsResponse.Jobs.
func (a *Agent) startJob(ctx context.Context, w http.ResponseWriter, containerID, action string) {
	res, err := a.docker.api.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		writeJSON(w, types.CommandResponse{Success: false, Message: err.Error()})
		return
	}
	info := res.Container
	name := strings.TrimPrefix(info.Name, "/")
	oldImageID := info.Image

	run := func(jctx context.Context) error {
		if action == "pull" {
			return a.docker.PullImage(jctx, info.ID)
		}
		if err := a.docker.UpgradeContainer(jctx, info.ID); err != nil {
			return err
		}
		a.updateCacheMu.Lock()
		delete(a.updateCache, oldImageID)
		a.updateCacheMu.Unlock()
		if err := a.docker.PruneImages(jctx); err != nil {
			log.Printf("prune images after upgrading %s: %v", name, err)
		}
		return nil
	}

	err = a.jobs.start(name, action, run, a.onJobDone)
	if errors.Is(err, errJobRunning) {
		writeJSON(w, types.CommandResponse{Success: false, Message: fmt.Sprintf("an upgrade or pull is already running for %s", name)})
		return
	}
	writeJSON(w, types.CommandResponse{Success: true, Pending: true, Message: action + " started"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func getHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
