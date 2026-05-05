package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jester/sddb/internal/types"
)

// Poller periodically fetches stats from all known agents.
type Poller struct {
	state     *State
	interval  time.Duration
	notify    chan struct{} // written to on every successful poll
	tlsConfig *tls.Config  // nil → plain HTTP
}

func NewPoller(state *State, interval time.Duration, notify chan struct{}, tlsConfig *tls.Config) *Poller {
	return &Poller{state: state, interval: interval, notify: notify, tlsConfig: tlsConfig}
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
	select {
	case p.notify <- struct{}{}:
	default:
	}
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
	var transport http.RoundTripper
	if p.tlsConfig != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: p.tlsConfig}
	}

	url := fmt.Sprintf("%s://%s/api/containers", scheme, addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return types.StatsResponse{}, err
	}

	client := &http.Client{Timeout: 10 * time.Second, Transport: transport}
	resp, err := client.Do(req)
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
