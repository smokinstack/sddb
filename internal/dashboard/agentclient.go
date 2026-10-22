package dashboard

import (
	"crypto/tls"
	"net/http"
	"time"
)

// newAgentTransport returns the transport used for all dashboard → agent
// requests. It must be created once and shared: a new http.Transport per
// request keeps its idle keep-alive connection (and two goroutines) open
// forever, on both the dashboard and the agent. The live log viewer polls
// every 3s, so that used to leak ~1,200 connections an hour.
func newAgentTransport(tlsCfg *tls.Config) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = tlsCfg
	t.MaxIdleConns = 32
	t.MaxIdleConnsPerHost = 4
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// newAgentClient returns an http.Client for agent requests. It has no overall
// timeout; callers bound each request with a context deadline instead, since
// different requests (logs, commands) need different limits.
func newAgentClient(tlsCfg *tls.Config) *http.Client {
	return &http.Client{Transport: newAgentTransport(tlsCfg)}
}

func agentScheme(tlsCfg *tls.Config) string {
	if tlsCfg != nil {
		return "https"
	}
	return "http"
}
