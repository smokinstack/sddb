package dashboard

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Repeated log/command requests must reuse connections. Previously every
// request built its own http.Transport, so each one opened a new connection
// that was never closed (the live log view polls every 3 seconds).
func TestAgentRequestsReuseConnections(t *testing.T) {
	var newConns atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("log line\n"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			newConns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	d := newTestDashboard(t, addr)

	const requests = 50
	for i := 0; i < requests; i++ {
		rec := httptest.NewRecorder()
		d.handleLogs(rec, httptest.NewRequest(http.MethodGet, "/api/logs?agent="+addr+"&container=web", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
	}

	if n := newConns.Load(); n > 2 {
		t.Fatalf("%d requests opened %d connections; expected the shared client to reuse one", requests, n)
	}
}
