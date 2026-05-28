package dashboard

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

type containerKey struct {
	agentAddr string
	name      string
}

type alertStatus int

const (
	statusNormal     alertStatus = iota
	statusSuppressed             // crash-loop detected — silenced until recovery
)

type containerAlert struct {
	status alertStatus

	// Previous state (for change detection)
	prevState    string
	prevRestarts int

	// Rolling window of crash event timestamps (for loop detection)
	crashTimes []time.Time

	// Cooldown: when did we last send a normal (non-loop) alert?
	lastAlert time.Time

	// Stability tracking: first moment the container was continuously stable
	stableFrom time.Time
}

const (
	loopThreshold    = 2               // crash events within loopWindow triggers suppression
	loopWindow       = 15 * time.Minute
	alertCooldown    = 5 * time.Minute  // minimum gap between individual crash alerts
	recoveryDuration = 10 * time.Minute // stable run required before clearing suppression
)

// Notifier watches container states across polls and sends ntfy alerts.
type Notifier struct {
	cfg   *config.Store
	mu    sync.Mutex
	state map[containerKey]*containerAlert
	http  *http.Client
}

func newNotifier(cfg *config.Store) *Notifier {
	return &Notifier{
		cfg:   cfg,
		state: make(map[containerKey]*containerAlert),
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *Notifier) Check(agentAddr string, containers []types.ContainerState) {
	cfg := n.cfg.Get()
	ntfyURL := cfg.NtfyURL
	// Master kill-switch or per-host mute: still track state but send nothing.
	muted := cfg.NtfyDisabled || cfg.NtfyDisabledHosts[agentAddr]

	n.mu.Lock()
	defer n.mu.Unlock()

	for _, c := range containers {
		key := containerKey{agentAddr, c.Name}
		a, seen := n.state[key]
		if !seen {
			n.state[key] = &containerAlert{
				prevState:    c.State,
				prevRestarts: c.RestartCount,
			}
			continue
		}

		prevState := a.prevState
		prevRestarts := a.prevRestarts
		a.prevState = c.State
		a.prevRestarts = c.RestartCount

		// ── Stability tracking ────────────────────────────────────────────────
		// A container is stable when it is running and hasn't restarted.
		nowStable := c.State == "running" && c.RestartCount == prevRestarts
		if nowStable {
			if a.stableFrom.IsZero() {
				a.stableFrom = time.Now()
			}
		} else {
			a.stableFrom = time.Time{}
		}

		// ── Recovery check ────────────────────────────────────────────────────
		if a.status == statusSuppressed && !a.stableFrom.IsZero() &&
			time.Since(a.stableFrom) >= recoveryDuration {

			a.status = statusNormal
			a.crashTimes = nil
			if ntfyURL != "" && !muted {
				go n.send(ntfyURL,
					fmt.Sprintf("Recovered: %s", c.Name),
					fmt.Sprintf("Container has been stable for %s on %s.", recoveryDuration, agentAddr),
					"low")
			}
		}

		// ── Crash / restart event detection ───────────────────────────────────
		var title, msg, priority string
		isCrash := false

		switch {
		case c.RestartCount > prevRestarts:
			isCrash = true
			if c.OOMKilled {
				title = fmt.Sprintf("OOMKilled: %s", c.Name)
				msg = fmt.Sprintf("OOM-killed on %s and restarted.\nTotal restarts: %d", agentAddr, c.RestartCount)
				priority = "high"
			} else {
				title = fmt.Sprintf("Restarted: %s", c.Name)
				msg = fmt.Sprintf("Restarted on %s (exit %d).\nTotal restarts: %d", agentAddr, c.ExitCode, c.RestartCount)
				priority = "default"
			}

		case prevState == "running" && (c.State == "exited" || c.State == "dead"):
			if c.OOMKilled {
				isCrash = true
				title = fmt.Sprintf("OOMKilled: %s", c.Name)
				msg = fmt.Sprintf("OOM-killed on %s and stopped.", agentAddr)
				priority = "high"
			} else if c.ExitCode != 0 {
				isCrash = true
				title = fmt.Sprintf("Crashed: %s", c.Name)
				msg = fmt.Sprintf("Crashed on %s with exit %d.", agentAddr, c.ExitCode)
				priority = "default"
			}
			// Exit 0 = intentional stop — no alert
		}

		if !isCrash || ntfyURL == "" || muted {
			continue
		}

		// Record crash event timestamp for loop detection
		now := time.Now()
		pruned := a.crashTimes[:0]
		for _, t := range a.crashTimes {
			if now.Sub(t) < loopWindow {
				pruned = append(pruned, t)
			}
		}
		a.crashTimes = append(pruned, now)

		// If already suppressed, say nothing
		if a.status == statusSuppressed {
			continue
		}

		// Crash-loop check: N or more events within the window
		if len(a.crashTimes) >= loopThreshold {
			a.status = statusSuppressed
			go n.send(ntfyURL,
				fmt.Sprintf("Crash loop: %s", c.Name),
				fmt.Sprintf("%s is crash-looping on %s (%d crashes in %s).\n"+
					"Alerts suppressed until container is stable for %s.",
					c.Name, agentAddr, len(a.crashTimes), loopWindow, recoveryDuration),
				"urgent")
			continue
		}

		// Normal single-event alert, subject to cooldown
		if time.Since(a.lastAlert) < alertCooldown {
			continue
		}
		a.lastAlert = now
		go n.send(ntfyURL, title, msg, priority)
	}
}

func (n *Notifier) send(url, title, message, priority string) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(message))
	if err != nil {
		log.Printf("ntfy: build request: %v", err)
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Content-Type", "text/plain")
	if priority != "" {
		req.Header.Set("Priority", priority)
	}
	resp, err := n.http.Do(req)
	if err != nil {
		log.Printf("ntfy: send failed: %v", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("ntfy: server returned %d for %q", resp.StatusCode, title)
	}
}
