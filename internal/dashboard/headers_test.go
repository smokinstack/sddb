package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/auth"
	"github.com/smokinstack/sddb/internal/config"
)

// Every response, including the login page, redirects and static files,
// carries the security headers.
func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := NewState("")
	d, err := NewDashboard(state, NewPoller(state, time.Hour, nil, cfg), os.DirFS("../../cmd/dashboard/web"),
		Config{Cfg: cfg, Sessions: auth.NewSessions()})
	if err != nil {
		t.Fatal(err)
	}
	h := d.Handler()

	for _, path := range []string{"/", "/login", "/api/main", "/static/app.css"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		hdr := rec.Header()
		if !strings.Contains(hdr.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP = %q, want frame-ancestors 'none'", path, hdr.Get("Content-Security-Policy"))
		}
		for name, want := range map[string]string{
			"X-Frame-Options":        "DENY",
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "same-origin",
		} {
			if got := hdr.Get(name); got != want {
				t.Errorf("%s: %s = %q, want %q", path, name, got, want)
			}
		}
	}
}
