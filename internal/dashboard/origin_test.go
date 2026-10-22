package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/smokinstack/sddb/internal/auth"
)

func TestSameOrigin(t *testing.T) {
	d := &Dashboard{trustedOrigins: []string{"https://sddb.example.com"}}
	h := d.sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		name    string
		method  string
		headers map[string]string
		want    int
	}{
		{"same origin (how jester uses it)", "POST", map[string]string{"Origin": "http://192.168.0.76:8069"}, 204},
		{"other app on the same host", "POST", map[string]string{"Origin": "http://192.168.0.76:3000"}, 403},
		{"other site", "DELETE", map[string]string{"Origin": "https://evil.example"}, 403},
		{"sandboxed / privacy origin", "POST", map[string]string{"Origin": "null"}, 403},
		{"referer only, same origin", "PATCH", map[string]string{"Referer": "http://192.168.0.76:8069/"}, 204},
		{"referer only, other origin", "POST", map[string]string{"Referer": "http://192.168.0.76:3000/d/x"}, 403},
		{"non-browser client", "POST", nil, 204},
		{"behind a proxy that sets X-Forwarded-Host", "POST", map[string]string{"Origin": "https://dash.lan", "X-Forwarded-Host": "dash.lan"}, 204},
		{"explicitly trusted origin", "POST", map[string]string{"Origin": "https://sddb.example.com"}, 204},
		{"reads are never blocked", "GET", map[string]string{"Origin": "https://evil.example"}, 204},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(c.method, "http://192.168.0.76:8069/api/command", strings.NewReader("{}"))
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != c.want {
				t.Fatalf("status %d, want %d", rec.Code, c.want)
			}
		})
	}
}

// End to end: a cross-site page can't stop a container even when the
// browser attaches a valid session cookie.
func TestCrossSiteCommandBlockedWithValidSession(t *testing.T) {
	agent, upgrades := upgradeAgent(t, true)
	addr := strings.TrimPrefix(agent.URL, "http://")
	d := newTestDashboard(t, addr)
	d.state.UpdateFromPoll(addr, statsWith("agent-1", webWithUpdate...))
	d.sessions = auth.NewSessions()
	token := d.sessions.Create()
	handler := d.Handler()

	body := `{"agent_addr":"` + addr + `","container_id":"web","action":"upgrade"}`
	r := httptest.NewRequest("POST", "http://192.168.0.76:8069/api/command", strings.NewReader(body))
	r.Header.Set("Origin", "http://192.168.0.76:3000")
	r.AddCookie(&http.Cookie{Name: "sddb_session", Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden || upgrades.Load() != 0 {
		t.Fatalf("status %d, %d upgrades sent; want 403 and none", rec.Code, upgrades.Load())
	}
}
