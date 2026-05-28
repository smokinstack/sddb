package dashboard

import (
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

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i != -1 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}
