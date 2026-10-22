package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/types"
)

type fakeRegistry struct {
	mu      sync.Mutex
	checks  map[string]int  // image name → registry checks
	updates map[string]bool // image name → update available
	fail    map[string]error
}

func (f *fakeRegistry) check(ctx context.Context, image string, digests []string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[image]++
	if err := f.fail[image]; err != nil {
		return false, err
	}
	return f.updates[image], nil
}

func newUpdateTestAgent(containers ...types.ContainerState) (*Agent, *fakeRegistry, *time.Time) {
	reg := &fakeRegistry{checks: map[string]int{}, updates: map[string]bool{}, fail: map[string]error{}}
	a := New(Config{}, nil)
	now := time.Unix(1_700_000_000, 0)
	a.now = func() time.Time { return now }
	a.repoDigests = func(ctx context.Context, id string) ([]string, error) { return []string{"x@" + id}, nil }
	a.checkRegistry = reg.check
	a.lastStats.Containers = containers
	return a, reg, &now
}

func TestUpdateChecksAreCachedPerImage(t *testing.T) {
	a, reg, now := newUpdateTestAgent(
		types.ContainerState{Name: "plex", Image: "lscr.io/linuxserver/plex:latest", ImageID: "sha256:p"},
		types.ContainerState{Name: "web1", Image: "nginx", ImageID: "sha256:n"},
		types.ContainerState{Name: "web2", Image: "nginx", ImageID: "sha256:n"}, // same image
	)
	reg.updates["lscr.io/linuxserver/plex:latest"] = true

	a.checkUpdatesOnce(context.Background())
	if reg.checks["lscr.io/linuxserver/plex:latest"] != 1 || reg.checks["nginx"] != 1 {
		t.Fatalf("checks = %v, want one per image", reg.checks)
	}
	if !a.updateAvailable("sha256:p") || a.updateAvailable("sha256:n") {
		t.Fatal("cached results don't match the registry")
	}

	// Within the interval: served from cache, no registry traffic.
	*now = now.Add(updateCheckInterval - time.Second)
	a.checkUpdatesOnce(context.Background())
	if reg.checks["nginx"] != 1 {
		t.Fatalf("re-checked within the interval: %v", reg.checks)
	}

	// After it: checked again.
	*now = now.Add(2 * time.Second)
	a.checkUpdatesOnce(context.Background())
	if reg.checks["nginx"] != 2 {
		t.Fatalf("not re-checked after the interval: %v", reg.checks)
	}
}

func TestUpdateCheckSkipsImageIDContainers(t *testing.T) {
	a, reg, _ := newUpdateTestAgent(
		types.ContainerState{Name: "portainer_agent", Image: "sha256:a599effa096f", ImageID: "sha256:a599effa096f"},
	)
	a.checkUpdatesOnce(context.Background())
	if len(reg.checks) != 0 {
		t.Fatalf("registry queried for a container created from an image ID: %v", reg.checks)
	}
}

func TestUpdateCheckKeepsLastResultOnError(t *testing.T) {
	a, reg, now := newUpdateTestAgent(types.ContainerState{Name: "app", Image: "example/app", ImageID: "sha256:a"})
	reg.updates["example/app"] = true
	a.checkUpdatesOnce(context.Background())

	reg.fail["example/app"] = errors.New("registry unreachable")
	*now = now.Add(updateCheckInterval + time.Second)
	a.checkUpdatesOnce(context.Background())
	if !a.updateAvailable("sha256:a") {
		t.Fatal("a failed check wiped a known update")
	}
}

func TestUpdateCacheForgetsRemovedImages(t *testing.T) {
	a, _, _ := newUpdateTestAgent(types.ContainerState{Name: "old", Image: "example/old", ImageID: "sha256:old"})
	a.checkUpdatesOnce(context.Background())

	a.lastStats.Containers = []types.ContainerState{{Name: "new", Image: "example/new", ImageID: "sha256:new"}}
	a.checkUpdatesOnce(context.Background())

	a.updateCacheMu.Lock()
	defer a.updateCacheMu.Unlock()
	if _, ok := a.updateCache["sha256:old"]; ok || len(a.updateCache) != 1 {
		t.Fatalf("cache = %v, want only the image still in use", a.updateCache)
	}
}

// A slow registry must not hold up anything else: the check runs in its own
// loop, and refresh only reads the cache.
func TestSlowRegistryDoesNotBlockCacheReads(t *testing.T) {
	a, _, _ := newUpdateTestAgent(types.ContainerState{Name: "app", Image: "example/app", ImageID: "sha256:a"})
	release := make(chan struct{})
	a.checkRegistry = func(ctx context.Context, image string, d []string) (bool, error) {
		<-release
		return true, nil
	}
	done := make(chan struct{})
	go func() { a.checkUpdatesOnce(context.Background()); close(done) }()

	start := time.Now()
	for i := 0; i < 100; i++ {
		a.updateAvailable("sha256:a")
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("reading update status waited on the registry")
	}
	close(release)
	<-done
	if !a.updateAvailable("sha256:a") {
		t.Fatal("result not stored after the slow check finished")
	}
}
