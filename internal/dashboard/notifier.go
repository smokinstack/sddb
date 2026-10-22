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
	loopThreshold    = 2 // crash events within loopWindow triggers suppression
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

	// expected holds containers the dashboard itself just stopped,
	// restarted or upgraded: their exit is not a crash.
	expected map[containerKey]time.Time
	now      func() time.Time
	// deliver sends a notification (async in production; replaced in tests).
	deliver func(url, title, message, priority string)
}

// expectedStopWindow is how long after a dashboard action an exit is
// treated as intentional.
const expectedStopWindow = 2 * time.Minute

func newNotifier(cfg *config.Store) *Notifier {
	n := &Notifier{
		cfg:      cfg,
		state:    make(map[containerKey]*containerAlert),
		http:     &http.Client{Timeout: 10 * time.Second},
		expected: make(map[containerKey]time.Time),
		now:      time.Now,
	}
	n.deliver = func(url, title, message, priority string) { go n.send(url, title, message, priority) }
	return n
}

// ExpectStop records that the dashboard is about to stop, restart or upgrade
// a container, so the exit that follows isn't reported as a crash.
func (n *Notifier) ExpectStop(agentAddr, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	for k, until := range n.expected {
		if now.After(until) {
			delete(n.expected, k)
		}
	}
	n.expected[containerKey{agentAddr, name}] = now.Add(expectedStopWindow)
}

// intentionalExit reports whether an exit was a deliberate stop rather than a
// crash. Caller holds n.mu.
//
// `docker stop` sends SIGTERM and, after the timeout, SIGKILL. A process that
// doesn't handle SIGTERM exits 143 (128+15); that's a stop request, whoever
// sent it, so it's never reported. 137 (SIGKILL) is ambiguous: it's also what
// `docker kill` or the kernel produce, so it only counts as intentional right
// after the dashboard itself stopped the container. OOM kills are flagged
// separately and always reported.
func (n *Notifier) intentionalExit(key containerKey, exitCode int) bool {
	switch exitCode {
	case 143:
		return true
	case 137:
		until, ok := n.expected[key]
		return ok && n.now().Before(until)
	}
	// Any other code is the process failing on its own, even right after a
	// dashboard restart.
	return false
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
				n.deliver(ntfyURL,
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
			} else if c.ExitCode != 0 && !n.intentionalExit(key, c.ExitCode) {
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
			n.deliver(ntfyURL,
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
		n.deliver(ntfyURL, title, msg, priority)
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
