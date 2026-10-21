package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var registryHTTP = &http.Client{Timeout: 10 * time.Second}

// errAuthRequired means the registry needs real credentials (a private image).
var errAuthRequired = errors.New("registry requires authentication")

const manifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.oci.image.manifest.v1+json"

// CheckUpdateAvailable returns true when the registry has a newer digest than
// what is recorded in repoDigests (the digests stored at last pull time).
// Works with any registry that speaks the OCI distribution spec, including
// Docker Hub, ghcr.io and lscr.io, using the anonymous token flow announced in
// the registry's WWW-Authenticate challenge.
//
// It returns an error when the check could not be performed (network error,
// private registry, unexpected response), so the caller can log it instead of
// it looking like "no update".
func CheckUpdateAvailable(ctx context.Context, imageName string, repoDigests []string) (bool, error) {
	ref, err := parseImageRef(imageName)
	if err != nil {
		return false, err
	}

	localDigest := extractLocalDigest(repoDigests, ref)
	if localDigest == "" {
		return false, nil // never pulled from a registry, can't compare
	}

	remoteDigest, err := fetchRemoteDigest(ctx, ref)
	if err != nil {
		return false, err
	}
	if remoteDigest == "" {
		return false, fmt.Errorf("registry returned no Docker-Content-Digest for %s", imageName)
	}

	if remoteDigest != localDigest {
		log.Printf("digest mismatch for %s: remote=%s local=%s", imageName, remoteDigest, localDigest)
	}
	return remoteDigest != localDigest, nil
}

type imageRef struct {
	registry string
	name     string // repository path, e.g. "library/nginx"
	tag      string
}

func parseImageRef(s string) (imageRef, error) {
	// strip digest if present
	if i := strings.Index(s, "@"); i != -1 {
		s = s[:i]
	}
	if s == "" {
		return imageRef{}, errors.New("empty image reference")
	}

	var ref imageRef

	// Split off tag
	if i := strings.LastIndex(s, ":"); i != -1 && !strings.Contains(s[i:], "/") {
		ref.tag = s[i+1:]
		s = s[:i]
	} else {
		ref.tag = "latest"
	}

	ref.registry, ref.name = splitRepo(s)
	return ref, nil
}

// splitRepo splits a repository (no tag/digest) into registry host and path,
// applying Docker Hub defaults.
func splitRepo(s string) (registry, name string) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		registry, name = parts[0], parts[1]
	} else {
		registry = "registry-1.docker.io"
		if len(parts) == 1 {
			name = "library/" + parts[0]
		} else {
			name = s
		}
	}
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = "registry-1.docker.io"
		if !strings.Contains(name, "/") {
			name = "library/" + name
		}
	}
	return registry, name
}

func fetchRemoteDigest(ctx context.Context, ref imageRef) (string, error) {
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.registry, ref.name, ref.tag)

	// First attempt is anonymous. Most registries (Docker Hub, ghcr.io, lscr.io)
	// answer 401 with a Bearer challenge telling us where to get a token.
	resp, err := doManifestRequest(ctx, manifestURL, "")
	if err != nil {
		return "", err
	}

	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		// Use the URL we actually ended up at: lscr.io redirects to ghcr.io,
		// and the token must be presented to the host that issued the challenge.
		finalURL := resp.Request.URL.String()
		resp.Body.Close()

		token, err := fetchChallengeToken(ctx, challenge, ref.name)
		if err != nil {
			return "", err
		}
		resp, err = doManifestRequest(ctx, finalURL, token)
		if err != nil {
			return "", err
		}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		return resp.Header.Get("Docker-Content-Digest"), nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", fmt.Errorf("%w (HTTP %d from %s)", errAuthRequired, resp.StatusCode, resp.Request.URL.Host)
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", fmt.Errorf("registry rate limit hit (HTTP 429 from %s)", resp.Request.URL.Host)
	default:
		return "", fmt.Errorf("registry returned HTTP %d from %s", resp.StatusCode, resp.Request.URL.Host)
	}
}

// doManifestRequest sends a HEAD for the manifest (HEAD does not count against
// Docker Hub pull limits), falling back to GET if HEAD is not supported.
func doManifestRequest(ctx context.Context, manifestURL, token string) (*http.Response, error) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, manifestURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", manifestAccept)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := registryHTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if method == http.MethodHead && (resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented) {
			resp.Body.Close()
			continue
		}
		return resp, nil
	}
	return nil, errors.New("unreachable")
}

// fetchChallengeToken obtains an anonymous pull token from the realm given in a
// "WWW-Authenticate: Bearer realm=...,service=...,scope=..." challenge.
func fetchChallengeToken(ctx context.Context, challenge, repo string) (string, error) {
	scheme, params := parseAuthChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		// Basic auth or no challenge at all: needs real credentials.
		return "", fmt.Errorf("%w (challenge %q)", errAuthRequired, challenge)
	}

	u, err := url.Parse(params["realm"])
	if err != nil {
		return "", fmt.Errorf("bad token realm %q: %w", params["realm"], err)
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + repo + ":pull"
	}
	q.Set("scope", scope)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w (token endpoint %s returned HTTP %d)", errAuthRequired, u.Host, resp.StatusCode)
	}

	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if body.Token != "" {
		return body.Token, nil
	}
	if body.AccessToken != "" {
		return body.AccessToken, nil
	}
	return "", fmt.Errorf("token endpoint %s returned no token", u.Host)
}

// parseAuthChallenge parses `Bearer realm="x",service="y",scope="z"`.
func parseAuthChallenge(h string) (scheme string, params map[string]string) {
	params = map[string]string{}
	h = strings.TrimSpace(h)
	sp := strings.IndexByte(h, ' ')
	if sp == -1 {
		return h, params
	}
	scheme, rest := h[:sp], h[sp+1:]

	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		eq := strings.IndexByte(rest, '=')
		if eq == -1 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(rest[:eq]))
		rest = rest[eq+1:]

		var val string
		if strings.HasPrefix(rest, `"`) {
			end := strings.IndexByte(rest[1:], '"')
			if end == -1 {
				val, rest = rest[1:], ""
			} else {
				val, rest = rest[1:1+end], rest[2+end:]
			}
		} else {
			end := strings.IndexByte(rest, ',')
			if end == -1 {
				val, rest = rest, ""
			} else {
				val, rest = rest[:end], rest[end:]
			}
		}
		params[key] = strings.TrimSpace(val)
	}
	return scheme, params
}

// extractLocalDigest finds the digest for the given registry+name in the list
// of RepoDigests returned by Docker (format: "name@sha256:..."). If no entry
// matches the repository exactly (e.g. the image was also pulled under another
// name), it falls back to the first digest.
func extractLocalDigest(repoDigests []string, ref imageRef) string {
	var first string
	for _, d := range repoDigests {
		i := strings.Index(d, "@")
		if i == -1 {
			continue
		}
		digest := d[i+1:]
		if first == "" {
			first = digest
		}
		reg, name := splitRepo(d[:i])
		if reg == ref.registry && name == ref.name {
			return digest
		}
	}
	return first
}
