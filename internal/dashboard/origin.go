package dashboard

import (
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// sameOrigin rejects state-changing requests (POST, PUT, PATCH, DELETE) that a
// browser sent from another site.
//
// The session cookie is SameSite=Strict, but browsers treat every port on the
// same host as the same "site", so a page served by any other app on that
// host (Grafana, ntfy, …) could otherwise send requests here with the
// dashboard's cookie attached. Browsers always send Origin on cross-origin
// POSTs, and pages can't forge it, so checking it closes that gap.
//
// Requests without Origin or Referer come from non-browser clients and are
// let through; they still need a valid session.
func (d *Dashboard) sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		source := r.Header.Get("Origin")
		if source == "" {
			source = r.Header.Get("Referer")
		}
		if source == "" || originAllowed(source, r, d.trustedOrigins) {
			next.ServeHTTP(w, r)
			return
		}
		log.Printf("blocked cross-origin %s %s from %q", r.Method, r.URL.Path, source)
		http.Error(w, "cross-origin request blocked", http.StatusForbidden)
	})
}

// originAllowed reports whether source (an Origin or Referer value) is this
// dashboard: the Host the request was sent to, a host a reverse proxy passed
// on in X-Forwarded-Host, or an origin listed in SDDB_TRUSTED_ORIGINS.
func originAllowed(source string, r *http.Request, trusted []string) bool {
	u, err := url.Parse(source)
	if err != nil || u.Host == "" { // also rejects "null"
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	// Pages can't set X-Forwarded-Host on cross-site requests (forms can't
	// set headers, and fetch would need a CORS preflight we never approve),
	// so it can be trusted here.
	for _, v := range r.Header.Values("X-Forwarded-Host") {
		for _, h := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(h), u.Host) {
				return true
			}
		}
	}
	origin := u.Scheme + "://" + u.Host
	for _, t := range trusted {
		if strings.EqualFold(strings.TrimRight(t, "/"), origin) {
			return true
		}
	}
	return false
}

// trustedOriginsFromEnv reads SDDB_TRUSTED_ORIGINS: extra origins allowed to
// make changes, e.g. "https://sddb.example.com", for reverse proxies that
// rewrite the Host header without setting X-Forwarded-Host.
func trustedOriginsFromEnv() []string {
	var out []string
	for _, o := range strings.Split(os.Getenv("SDDB_TRUSTED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}
