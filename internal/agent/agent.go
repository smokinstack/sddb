package agent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/smokinstack/sddb/internal/types"
)

const version = "1.0.0"

// Config holds agent runtime configuration.
type Config struct {
	ID             string
	ListenAddr     string
	UpdateInterval time.Duration
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
}

type updateEntry struct {
	available bool
	checkedAt time.Time
}

func New(cfg Config, docker *DockerClient) *Agent {
	return &Agent{
		cfg:         cfg,
		docker:      docker,
		host:        &hostSampler{},
		updateCache: make(map[string]updateEntry),
	}
}

// Run starts the background refresh loop and the HTTP server.
// It blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	// initial collection
	if err := a.refresh(ctx); err != nil {
		log.Printf("initial stats collection failed: %v", err)
	}

	go a.refreshLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", a.handleInfo)
	mux.HandleFunc("/api/containers", a.handleContainers)
	mux.HandleFunc("/api/containers/", a.handleCommand) // /{id}/start|stop|restart|upgrade

	srv := &http.Server{
		Addr:      a.cfg.ListenAddr,
		Handler:   mux,
		TLSConfig: a.cfg.TLS,
	}

	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()

	if a.cfg.TLS != nil {
		log.Printf("agent %s listening on %s (TLS, interval %s)", a.cfg.ID, a.cfg.ListenAddr, a.cfg.UpdateInterval)
		// Cert and key are already embedded in TLSConfig via tls.X509KeyPair
		if err := srv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			return err
		}
	} else {
		log.Printf("agent %s listening on %s (interval %s)", a.cfg.ID, a.cfg.ListenAddr, a.cfg.UpdateInterval)
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

	// Check image updates (rate-limited: once per 5 minutes per image)
	for i := range containers {
		c := &containers[i]
		c.UpdateAvailable = a.checkUpdate(ctx, c.Image, c.ImageID)
	}

	hostname := getHostname()
	resp := types.StatsResponse{
		Agent: types.AgentInfo{
			ID:             a.cfg.ID,
			Hostname:       hostname,
			UpdateInterval: int(a.cfg.UpdateInterval.Seconds()),
			Version:        version,
		},
		Host:       a.host.sample(containers),
		Containers: containers,
		Timestamp:  time.Now().Unix(),
	}

	a.mu.Lock()
	a.lastStats = resp
	a.lastUpdate = time.Now()
	a.mu.Unlock()
	return nil
}

func (a *Agent) checkUpdate(ctx context.Context, imageName, imageID string) bool {
	const updateCheckInterval = 5 * time.Minute

	a.updateCacheMu.Lock()
	entry, ok := a.updateCache[imageID]
	a.updateCacheMu.Unlock()

	if ok && time.Since(entry.checkedAt) < updateCheckInterval {
		return entry.available
	}

	// Fetch RepoDigests for this image
	imgInfo, _, err := a.docker.cli.ImageInspectWithRaw(ctx, imageID)
	if err != nil {
		return false
	}

	available, err := CheckUpdateAvailable(ctx, imageName, imgInfo.RepoDigests)
	if err != nil {
		log.Printf("update check %s: %v", imageName, err)
	} else {
		log.Printf("update check %s: available=%v repoDigests=%v", imageName, available, imgInfo.RepoDigests)
	}

	a.updateCacheMu.Lock()
	a.updateCache[imageID] = updateEntry{available: available, checkedAt: time.Now()}
	a.updateCacheMu.Unlock()

	return available
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
	tail := r.URL.Query().Get("tail")
	if tail == "" {
		tail = "100"
	}

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

	timeout := 60 * time.Second
	if action == "upgrade" {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var err error
	switch action {
	case "start":
		err = a.docker.StartContainer(ctx, containerID)
	case "stop":
		err = a.docker.StopContainer(ctx, containerID)
	case "restart":
		err = a.docker.RestartContainer(ctx, containerID)
	case "upgrade":
		err = a.docker.UpgradeContainer(ctx, containerID)
		if err == nil {
			// Invalidate update cache for this container's image
			a.mu.RLock()
			for _, c := range a.lastStats.Containers {
				if c.ID == containerID || c.ShortID == containerID {
					a.updateCacheMu.Lock()
					delete(a.updateCache, c.ImageID)
					a.updateCacheMu.Unlock()
					break
				}
			}
			a.mu.RUnlock()
			// Prune dangling images in background
			go func() {
				a.docker.PruneImages(context.Background())
				// Force an immediate refresh
				a.refresh(context.Background())
			}()
		}
	case "pull":
		// Pull latest image without recreating the container
		a.mu.RLock()
		var imgName string
		for _, c := range a.lastStats.Containers {
			if c.ID == containerID || c.ShortID == containerID {
				imgName = c.Image
				break
			}
		}
		a.mu.RUnlock()
		if imgName != "" {
			reader, pullErr := a.docker.cli.ImagePull(ctx, imgName, image.PullOptions{})
			if pullErr == nil {
				reader.Close()
			}
			err = pullErr
		}
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
