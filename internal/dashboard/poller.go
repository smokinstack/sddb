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

	"github.com/jester/sddb/internal/config"
	"github.com/jester/sddb/internal/types"
)

// Poller periodically fetches stats from all known agents.
type Poller struct {
	state     *State
	interval  time.Duration
	notify    chan struct{} // written to on every successful poll
	tlsConfig *tls.Config  // nil → plain HTTP
	cfg       *config.Store

	// Shared HTTP clients — reused across all requests so connections are
	// pooled and ephemeral ports are not exhausted.
	pollClient    *http.Client
	upgradeClient *http.Client

	upgradeMu       sync.Mutex
	lastAutoUpgrade map[string]time.Time // "addr::name" → last upgrade time

	notifier *Notifier
}

func NewPoller(state *State, interval time.Duration, notify chan struct{}, tlsConfig *tls.Config, cfg *config.Store) *Poller {
	var transport http.RoundTripper
	if tlsConfig != nil {
		transport = &http.Transport{
			TLSClientConfig: tlsConfig,
			// Allow enough idle connections for all agents.
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 4,
		}
	}
	return &Poller{
		state:           state,
		interval:        interval,
		notify:          notify,
		tlsConfig:       tlsConfig,
		cfg:             cfg,
		pollClient:      &http.Client{Timeout: 10 * time.Second, Transport: transport},
		upgradeClient:   &http.Client{Timeout: 5 * time.Minute, Transport: transport},
		lastAutoUpgrade: make(map[string]time.Time),
		notifier:        newNotifier(cfg),
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
	go p.runAutoUpdates(addr, resp.Containers)
}

// runAutoUpdates upgrades any container that has auto-update enabled and an update available.
func (p *Poller) runAutoUpdates(addr string, containers []types.ContainerState) {
	const cooldown = 10 * time.Minute
	for _, c := range containers {
		if !c.UpdateAvailable || c.State != "running" {
			continue
		}
		if !p.cfg.IsAutoUpdate(addr, c.Name) {
			continue
		}
		key := addr + "::" + c.Name

		// Atomic check-and-claim: hold the lock through both the check and
		// the timestamp update so concurrent goroutines can't both pass.
		p.upgradeMu.Lock()
		last := p.lastAutoUpgrade[key]
		if time.Since(last) < cooldown {
			p.upgradeMu.Unlock()
			continue
		}
		p.lastAutoUpgrade[key] = time.Now() // claim the slot before releasing
		p.upgradeMu.Unlock()

		log.Printf("auto-update: upgrading %s on %s", c.Name, addr)
		if err := p.sendUpgrade(addr, c.ID); err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "toomanyrequests") || strings.Contains(errStr, "rate limit") {
				// Back off for 1 hour so we stop hammering the registry
				log.Printf("auto-update: rate limited upgrading %s on %s — backing off 1h", c.Name, addr)
				p.upgradeMu.Lock()
				p.lastAutoUpgrade[key] = time.Now().Add(-cooldown + time.Hour)
				p.upgradeMu.Unlock()
			} else {
				log.Printf("auto-update: upgrade %s on %s failed: %v", c.Name, addr, err)
				// Release the slot so it retries after the normal cooldown
				p.upgradeMu.Lock()
				p.lastAutoUpgrade[key] = last
				p.upgradeMu.Unlock()
			}
			continue
		}
		// Re-poll after a short delay so the dashboard reflects the change
		go func(a string) {
			time.Sleep(3 * time.Second)
			p.PollNow(context.Background(), a)
		}(addr)
	}
}

func (p *Poller) sendUpgrade(addr, containerID string) error {
	scheme := "http"
	if p.tlsConfig != nil {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s/api/containers/%s/upgrade", scheme, addr, containerID)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.upgradeClient.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("agent returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
