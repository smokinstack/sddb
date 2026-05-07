package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

var registryHTTP = &http.Client{Timeout: 10 * time.Second}

// CheckUpdateAvailable returns true when the registry has a newer digest than
// what is recorded in repoDigests (the digests stored at last pull time).
// Supports Docker Hub and any registry that speaks the OCI distribution spec.
// Returns false (no error) when the check cannot be performed (e.g. private
// registry requires auth we don't have).
func CheckUpdateAvailable(ctx context.Context, imageName string, repoDigests []string) (bool, error) {
	ref, err := parseImageRef(imageName)
	if err != nil {
		return false, nil // best-effort
	}

	remoteDigest, err := fetchRemoteDigest(ctx, ref)
	if err != nil {
		return false, nil // network error or private registry → skip
	}
	if remoteDigest == "" {
		return false, nil
	}

	localDigest := extractLocalDigest(repoDigests, ref)
	if localDigest == "" {
		return false, nil // never pulled from a registry, can't compare
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

	var ref imageRef

	// Split off tag
	if i := strings.LastIndex(s, ":"); i != -1 && !strings.Contains(s[i:], "/") {
		ref.tag = s[i+1:]
		s = s[:i]
	} else {
		ref.tag = "latest"
	}

	// Detect registry (contains a dot or colon before the first slash)
	parts := strings.SplitN(s, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		ref.registry = parts[0]
		ref.name = parts[1]
	} else {
		ref.registry = "registry-1.docker.io"
		if len(parts) == 1 {
			ref.name = "library/" + parts[0]
		} else {
			ref.name = s
		}
	}

	return ref, nil
}

func fetchRemoteDigest(ctx context.Context, ref imageRef) (string, error) {
	token, err := fetchRegistryToken(ctx, ref)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", ref.registry, ref.name, ref.tag)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	// Include manifest list types first so registries return the multi-arch
	// manifest list digest, which matches what Docker stores in RepoDigests.
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := registryHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", nil // private registry, skip
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned %d", resp.StatusCode)
	}

	return resp.Header.Get("Docker-Content-Digest"), nil
}

func fetchRegistryToken(ctx context.Context, ref imageRef) (string, error) {
	if ref.registry != "registry-1.docker.io" {
		return "", nil // only handle Docker Hub anonymously for now
	}

	url := fmt.Sprintf(
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull",
		ref.name,
	)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := registryHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return body.Token, nil
}

// extractLocalDigest finds the digest for the given registry+name in the list
// of RepoDigests returned by Docker (format: "name@sha256:...").
func extractLocalDigest(repoDigests []string, ref imageRef) string {
	for _, d := range repoDigests {
		// d looks like "nginx@sha256:abc..." or "registry/name@sha256:..."
		if i := strings.Index(d, "@"); i != -1 {
			return d[i+1:]
		}
	}
	return ""
}
