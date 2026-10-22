package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testRemoteDigest = "sha256:new"
	testLocalDigest  = "sha256:old"
)

// newFakeGHCR mimics ghcr.io: manifests need a bearer token obtained
// anonymously from /token.
func newFakeGHCR(t *testing.T, private bool) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if private {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Query().Get("scope") != "repository:linuxserver/plex:pull" {
				t.Errorf("unexpected scope %q", r.URL.Query().Get("scope"))
			}
			w.Write([]byte(`{"token":"anon"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/linuxserver/plex/manifests/"):
			if r.Header.Get("Authorization") != "Bearer anon" {
				w.Header().Set("WWW-Authenticate",
					`Bearer realm="`+srv.URL+`/token",service="ghcr.io",scope="repository:linuxserver/plex:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "image.index") {
				t.Errorf("Accept header missing index type: %q", r.Header.Get("Accept"))
			}
			w.Header().Set("Docker-Content-Digest", testRemoteDigest)
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// newFakeLSCR mimics lscr.io: redirects everything to ghcr.
func newFakeLSCR(ghcrURL string) *httptest.Server {
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, ghcrURL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
}

func useClient(t *testing.T, c *http.Client) {
	old := registryHTTP
	registryHTTP = c
	t.Cleanup(func() { registryHTTP = old })
}

func host(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "https://") }

func TestUpdateDetectedViaRedirectAndBearerChallenge(t *testing.T) {
	ghcr := newFakeGHCR(t, false)
	defer ghcr.Close()
	lscr := newFakeLSCR(ghcr.URL)
	defer lscr.Close()
	useClient(t, lscr.Client())

	image := host(lscr) + "/linuxserver/plex:latest"
	digests := []string{host(lscr) + "/linuxserver/plex@" + testLocalDigest}

	got, err := CheckUpdateAvailable(context.Background(), image, digests)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Fatal("expected update available")
	}

	// Up to date when digests match.
	digests = []string{host(lscr) + "/linuxserver/plex@" + testRemoteDigest}
	got, err = CheckUpdateAvailable(context.Background(), image, digests)
	if err != nil || got {
		t.Fatalf("expected no update, got %v, %v", got, err)
	}
}

func TestPrivateRegistryReturnsError(t *testing.T) {
	ghcr := newFakeGHCR(t, true)
	defer ghcr.Close()
	useClient(t, ghcr.Client())

	_, err := CheckUpdateAvailable(context.Background(),
		host(ghcr)+"/linuxserver/plex:latest",
		[]string{host(ghcr) + "/linuxserver/plex@" + testLocalDigest})
	if !errors.Is(err, errAuthRequired) {
		t.Fatalf("expected errAuthRequired, got %v", err)
	}
}

func TestParseAuthChallenge(t *testing.T) {
	s, p := parseAuthChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" ||
		p["service"] != "registry.docker.io" || p["scope"] != "repository:library/nginx:pull" {
		t.Fatalf("got %q %v", s, p)
	}
}

func TestExtractLocalDigestPrefersMatchingRepo(t *testing.T) {
	ref, _ := parseImageRef("lscr.io/linuxserver/plex:latest")
	got := extractLocalDigest([]string{
		"ghcr.io/linuxserver/plex@sha256:aaa",
		"lscr.io/linuxserver/plex@sha256:bbb",
	}, ref)
	if got != "sha256:bbb" {
		t.Fatalf("got %s", got)
	}

	ref, _ = parseImageRef("nginx")
	if got := extractLocalDigest([]string{"nginx@sha256:ccc"}, ref); got != "sha256:ccc" {
		t.Fatalf("got %s", got)
	}
}
