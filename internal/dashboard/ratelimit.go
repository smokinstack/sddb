package dashboard

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxFailures   = 5
	failureWindow = 15 * time.Minute
	lockoutPeriod = 15 * time.Minute
)

type ipEntry struct {
	failures    int
	firstFail   time.Time
	lockedUntil time.Time
}

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*ipEntry
}

func newLoginLimiter() *loginLimiter {
	l := &loginLimiter{entries: make(map[string]*ipEntry)}
	go l.reapLoop()
	return l
}

func (l *loginLimiter) allowed(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		return true
	}
	if time.Now().Before(e.lockedUntil) {
		return false
	}
	if !e.lockedUntil.IsZero() {
		delete(l.entries, ip)
	}
	return true
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[ip]
	if !ok {
		e = &ipEntry{firstFail: time.Now()}
		l.entries[ip] = e
	}
	if time.Since(e.firstFail) > failureWindow {
		e.failures = 0
		e.firstFail = time.Now()
	}
	e.failures++
	if e.failures >= maxFailures {
		e.lockedUntil = time.Now().Add(lockoutPeriod)
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	delete(l.entries, ip)
	l.mu.Unlock()
}

func (l *loginLimiter) reapLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		l.mu.Lock()
		for ip, e := range l.entries {
			if e.lockedUntil.IsZero() && now.Sub(e.firstFail) > failureWindow {
				delete(l.entries, ip)
			} else if !e.lockedUntil.IsZero() && now.After(e.lockedUntil) {
				delete(l.entries, ip)
			}
		}
		l.mu.Unlock()
	}
}

// clientIP returns the address used for login rate limiting.
//
// Forwarded headers are only believed when the connection comes from a
// trusted proxy: loopback, or an address in SDDB_TRUSTED_PROXIES (e.g. the
// Docker network Caddy runs on). X-Forwarded-For is read from the right:
// proxies append the address they saw, so the rightmost entry that isn't
// itself a trusted proxy is the real client. The leftmost entry is whatever
// the client sent, so trusting it (as before) let anyone send a fresh fake
// address with every guess and never hit the lockout.
func clientIP(r *http.Request, trusted []*net.IPNet) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}
	if !isTrustedProxy(remoteIP, trusted) {
		return remoteIP
	}
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		hops := strings.Split(strings.Join(xff, ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop := strings.TrimSpace(hops[i])
			if hop == "" {
				continue
			}
			if !isTrustedProxy(hop, trusted) {
				return hop
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	return remoteIP
}

func isTrustedProxy(ip string, trusted []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	if parsed.IsLoopback() {
		return true
	}
	for _, n := range trusted {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// parseTrustedProxies parses a comma-separated list of IPs and CIDRs, as in
// SDDB_TRUSTED_PROXIES. Invalid entries are logged and skipped.
func parseTrustedProxies(list string) []*net.IPNet {
	var nets []*net.IPNet
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if !strings.Contains(item, "/") {
			if ip := net.ParseIP(item); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				item = fmt.Sprintf("%s/%d", item, bits)
			}
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			log.Printf("SDDB_TRUSTED_PROXIES: ignoring %q: %v", item, err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}
