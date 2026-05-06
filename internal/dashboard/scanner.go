package dashboard

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jester/sddb/internal/types"
)

// ScanResult describes a discovered agent on the network.
type ScanResult struct {
	Addr     string          `json:"addr"`
	Info     types.AgentInfo `json:"info"`
	Reachable bool           `json:"reachable"`
}

// ScanNetwork scans the given CIDR for hosts listening on agentPort.
// Results are sent to the returned channel. The channel is closed when scanning completes.
func ScanNetwork(ctx context.Context, cidr string, agentPort int, concurrency int, tlsCfg *tls.Config) (<-chan ScanResult, error) {
	ips, err := hostsInCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("parse CIDR: %w", err)
	}

	results := make(chan ScanResult, len(ips))
	sem := make(chan struct{}, concurrency)

	var wg sync.WaitGroup
	for _, ip := range ips {
		ip := ip
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			addr := fmt.Sprintf("%s:%d", ip, agentPort)
			if result, ok := probeAgent(ctx, addr, tlsCfg); ok {
				select {
				case results <- result:
				case <-ctx.Done():
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	return results, nil
}

func probeAgent(ctx context.Context, addr string, tlsCfg *tls.Config) (ScanResult, bool) {
	// Quick TCP dial first (much faster than HTTP timeout)
	dialCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()

	d := net.Dialer{}
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return ScanResult{}, false
	}
	conn.Close()

	// TCP is open — now verify it's actually an sddb agent
	scheme := "http"
	var transport http.RoundTripper
	if tlsCfg != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: tlsCfg}
	}
	url := fmt.Sprintf("%s://%s/api/info", scheme, addr)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	client := &http.Client{Timeout: 2 * time.Second, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return ScanResult{}, false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ScanResult{}, false
	}

	var info types.AgentInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return ScanResult{}, false
	}

	return ScanResult{Addr: addr, Info: info, Reachable: true}, true
}

// LocalCIDR returns the CIDR of the first non-loopback network interface.
func LocalCIDR() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				return ipnet.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no suitable network interface found")
}

func hostsInCIDR(cidr string) ([]string, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, err
	}

	var ips []string
	for ip := cloneIP(ipnet.IP); ipnet.Contains(ip); incrementIP(ip) {
		// skip network address and broadcast
		if ip.Equal(ipnet.IP) {
			continue
		}
		broadcast := broadcastAddr(ipnet)
		if ip.Equal(broadcast) {
			continue
		}
		ips = append(ips, ip.String())
	}
	return ips, nil
}

func cloneIP(ip net.IP) net.IP {
	clone := make(net.IP, len(ip))
	copy(clone, ip)
	return clone
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func broadcastAddr(n *net.IPNet) net.IP {
	broadcast := make(net.IP, len(n.IP))
	for i := range n.IP {
		broadcast[i] = n.IP[i] | ^n.Mask[i]
	}
	return broadcast
}
