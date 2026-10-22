package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

// Poller periodically fetches stats from all known agents.
type Poller struct {
	state     *State
	interval  time.Duration
	notify    chan struct{} // written to on every successful poll
	tlsConfig *tls.Config   // nil → plain HTTP
	cfg       *config.Store

	// Shared HTTP clients — reused across all requests so connections are
	// pooled and ephemeral ports are not exhausted.
	pollClient    *http.Client
	upgradeClient *http.Client

	upgradeMu sync.Mutex
	auto      map[string]*autoUpdateState // "addr::name" → auto-update bookkeeping
	now       func() time.Time            // clock; overridable in tests

	notifier *Notifier
}

// Auto-update pacing. After a successful trigger a container is left alone
// for autoCooldown. After a failure the wait doubles each time (10m, 20m,
// 40m, … up to maxAutoBackoff) so a container that can never upgrade isn't
// retried every poll; registry rate limits wait at least rateLimitBackoff.
const (
	autoCooldown     = 10 * time.Minute
	maxAutoBackoff   = 6 * time.Hour
	rateLimitBackoff = time.Hour
)

type autoUpdateState struct {
	next         time.Time // no new attempt before this
	failures     int       // consecutive failed upgrades
	seenFinished int64     // agent job Finished time already accounted for
}

func failureBackoff(failures int, errMsg string) time.Duration {
	d := maxAutoBackoff
	if failures >= 1 && failures < 16 {
		d = min(autoCooldown<<(failures-1), maxAutoBackoff)
	}
	if strings.Contains(errMsg, "toomanyrequests") || strings.Contains(errMsg, "rate limit") {
		d = max(d, rateLimitBackoff)
	}
	return d
}

func NewPoller(state *State, interval time.Duration, notify chan struct{}, tlsConfig *tls.Config, cfg *config.Store) *Poller {
	transport := newAgentTransport(tlsConfig)
	return &Poller{
		state:      state,
		interval:   interval,
		notify:     notify,
		tlsConfig:  tlsConfig,
		cfg:        cfg,
		pollClient: &http.Client{Timeout: 10 * time.Second, Transport: transport},
		// Current agents start upgrades in the background and answer at once;
		// the long timeout only matters for older agents that upgrade
		// synchronously, where giving up early would kill the upgrade.
		upgradeClient: &http.Client{Timeout: 15 * time.Minute, Transport: transport},
		auto:          make(map[string]*autoUpdateState),
		now:           time.Now,
		notifier:      newNotifier(cfg),
	}
}

// Run starts polling all known agents in the background.
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.pollAll(ctx)
		}
	}
}

func (p *Poller) pollAll(ctx context.Context) {
	for _, addr := range p.state.Addresses() {
		go p.pollOne(ctx, addr)
	}
}

func (p *Poller) pollOne(ctx context.Context, addr string) {
	resp, err := p.fetchStats(ctx, addr)
	if err != nil {
		p.state.MarkOffline(addr, err.Error())
		log.Printf("poll %s: %v", addr, err)
		return
	}
	p.state.UpdateFromPoll(addr, resp)
	p.notifier.Check(addr, resp.Containers)
	select {
	case p.notify <- struct{}{}:
	default:
	}
	go p.runAutoUpdates(addr, resp.Containers, resp.Jobs)
}

// runAutoUpdates upgrades any container that has auto-update enabled and an
// update available. Upgrades run as background jobs on the agent; their
// outcome comes back in later polls via jobs.
func (p *Poller) runAutoUpdates(addr string, containers []types.ContainerState, jobs map[string]types.JobStatus) {
	for _, c := range containers {
		if !c.UpdateAvailable || c.State != "running" {
			continue
		}
		if p.cfg.IsProtected(c.Name) {
			continue
		}
		if !p.cfg.IsAutoUpdate(addr, c.Name) {
			continue
		}
		key := addr + "::" + c.Name

		// Check-and-claim under one lock so concurrent polls can't both fire.
		p.upgradeMu.Lock()
		st := p.auto[key]
		if st == nil {
			st = &autoUpdateState{}
			p.auto[key] = st
		}
		now := p.now()
		if j, ok := jobs[c.Name]; ok && j.Action == "upgrade" {
			if j.State == types.JobRunning {
				p.upgradeMu.Unlock()
				continue
			}
			if j.Finished > st.seenFinished {
				st.seenFinished = j.Finished
				if j.State == types.JobFailed {
					st.failures++
					wait := failureBackoff(st.failures, j.Error)
					st.next = now.Add(wait)
					log.Printf("auto-update: upgrade of %s on %s failed (%d in a row), next try in %s: %s",
						c.Name, addr, st.failures, wait, j.Error)
				} else {
					st.failures = 0
				}
			}
		}
		if now.Before(st.next) {
			p.upgradeMu.Unlock()
			continue
		}
		st.next = now.Add(autoCooldown) // claim the slot before releasing the lock
		p.upgradeMu.Unlock()

		log.Printf("auto-update: upgrading %s on %s", c.Name, addr)
		if err := p.sendUpgrade(addr, c.ID); err != nil {
			p.upgradeMu.Lock()
			st.failures++
			wait := failureBackoff(st.failures, err.Error())
			st.next = now.Add(wait)
			p.upgradeMu.Unlock()
			log.Printf("auto-update: could not start upgrade of %s on %s, next try in %s: %v", c.Name, addr, wait, err)
			continue
		}
		// Re-poll after a short delay so the dashboard shows the running job.
		go func(a string) {
			time.Sleep(3 * time.Second)
			p.PollNow(context.Background(), a)
		}(addr)
	}
}

// sendUpgrade asks the agent to start an upgrade job.
func (p *Poller) sendUpgrade(addr, containerID string) error {
	url := fmt.Sprintf("%s://%s/api/containers/%s/upgrade", agentScheme(p.tlsConfig), addr, containerID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.upgradeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var cr types.CommandResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return fmt.Errorf("decode agent response: %w", err)
	}
	if !cr.Success {
		return fmt.Errorf("agent refused upgrade: %s", cr.Message)
	}
	return nil
}

// PollNow immediately polls a single agent address.
func (p *Poller) PollNow(ctx context.Context, addr string) error {
	resp, err := p.fetchStats(ctx, addr)
	if err != nil {
		p.state.MarkOffline(addr, err.Error())
		return err
	}
	p.state.UpdateFromPoll(addr, resp)
	return nil
}

func (p *Poller) fetchStats(ctx context.Context, addr string) (types.StatsResponse, error) {
	scheme := "http"
	if p.tlsConfig != nil {
		scheme = "https"
	}

	url := fmt.Sprintf("%s://%s/api/containers", scheme, addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return types.StatsResponse{}, err
	}

	resp, err := p.pollClient.Do(req)
	if err != nil {
		return types.StatsResponse{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return types.StatsResponse{}, fmt.Errorf("agent returned %d", resp.StatusCode)
	}

	var stats types.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		return types.StatsResponse{}, fmt.Errorf("decode: %w", err)
	}
	return stats, nil
}
