package dashboard

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	dockerNet := parseTrustedProxies("172.16.0.0/12, 10.0.0.5")
	cases := []struct {
		name    string
		remote  string
		xff     string
		trusted bool
		want    string
	}{
		{"direct client, header ignored", "192.168.0.50:5555", "6.6.6.6", false, "192.168.0.50"},
		{"loopback proxy", "127.0.0.1:5555", "192.168.0.50", false, "192.168.0.50"},
		// Apache/nginx append the address they saw to whatever the client sent.
		{"spoofed entry is ignored", "127.0.0.1:5555", "6.6.6.6, 192.168.0.50", false, "192.168.0.50"},
		{"untrusted docker proxy (no SDDB_TRUSTED_PROXIES)", "172.18.0.2:5555", "192.168.0.50", false, "172.18.0.2"},
		{"trusted docker proxy (Caddy container)", "172.18.0.2:5555", "192.168.0.50", true, "192.168.0.50"},
		{"chain of trusted proxies", "172.18.0.2:5555", "6.6.6.6, 192.168.0.50, 10.0.0.5", true, "192.168.0.50"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/login", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			var trusted = dockerNet
			if !c.trusted {
				trusted = nil
			}
			if got := clientIP(r, trusted); got != c.want {
				t.Fatalf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	nets := parseTrustedProxies("172.16.0.0/12, 10.0.0.5 ,, ::1, not-an-ip")
	if len(nets) != 3 {
		t.Fatalf("parsed %d networks, want 3 (bad entries skipped): %v", len(nets), nets)
	}
}
