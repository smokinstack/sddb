package agent

import (
	"context"
	"log"
	"strings"
	"time"
)

const (
	updateCheckInterval = 5 * time.Minute // how often each image is re-checked
	updatePassInterval  = time.Minute     // how often the checker looks for due images
)

// Registry update checks run in their own loop, separate from the stats
// refresh. They used to run one by one inside refresh(), so a slow or
// unreachable registry (up to ~30s per image) stalled container stats, and
// the agent didn't start serving until the first round of checks finished.
// refresh() now only reads the cached results via updateAvailable.

// updateAvailable returns the cached update status for an image.
func (a *Agent) updateAvailable(imageID string) bool {
	a.updateCacheMu.Lock()
	defer a.updateCacheMu.Unlock()
	return a.updateCache[imageID].available
}

// updateCheckLoop checks images for updates every updatePassInterval, and
// straight away when kicked (e.g. after an upgrade replaced an image).
func (a *Agent) updateCheckLoop(ctx context.Context) {
	t := time.NewTicker(updatePassInterval)
	defer t.Stop()
	for {
		a.checkUpdatesOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kickUpdates:
		}
	}
}

// kickUpdateCheck asks the update loop to run a pass now, without blocking.
func (a *Agent) kickUpdateCheck() {
	select {
	case a.kickUpdates <- struct{}{}:
	default: // a pass is already pending
	}
}

// checkUpdatesOnce checks each image used by a container whose cached result
// is older than updateCheckInterval, and forgets images no longer in use.
func (a *Agent) checkUpdatesOnce(ctx context.Context) {
	a.mu.RLock()
	containers := a.lastStats.Containers
	a.mu.RUnlock()

	inUse := make(map[string]bool)
	for _, c := range containers {
		if c.ImageID == "" || inUse[c.ImageID] {
			continue
		}
		inUse[c.ImageID] = true

		a.updateCacheMu.Lock()
		entry, cached := a.updateCache[c.ImageID]
		a.updateCacheMu.Unlock()
		if cached && a.now().Sub(entry.checkedAt) < updateCheckInterval {
			continue
		}

		available := entry.available
		switch {
		case c.Image == "" || strings.HasPrefix(c.Image, "sha256:"):
			// Created from a bare image ID: there's no tag to compare against.
			available = false
		default:
			if result, ok := a.checkImage(ctx, c.Image, c.ImageID); ok {
				if !cached || result != entry.available {
					log.Printf("update check %s: available=%v", c.Image, result)
				}
				available = result
			}
		}

		a.updateCacheMu.Lock()
		a.updateCache[c.ImageID] = updateEntry{available: available, checkedAt: a.now()}
		a.updateCacheMu.Unlock()
	}

	a.updateCacheMu.Lock()
	for id := range a.updateCache {
		if !inUse[id] {
			delete(a.updateCache, id)
		}
	}
	a.updateCacheMu.Unlock()
}

// checkImage compares the local image with its registry. ok is false when
// the check couldn't be done; the previous result is then kept.
func (a *Agent) checkImage(ctx context.Context, imageName, imageID string) (available, ok bool) {
	digests, err := a.repoDigests(ctx, imageID)
	if err != nil {
		log.Printf("update check %s: inspect image: %v", imageName, err)
		return false, false
	}
	available, err = a.checkRegistry(ctx, imageName, digests)
	if err != nil {
		log.Printf("update check %s: %v", imageName, err)
		return false, false
	}
	return available, true
}

// dockerRepoDigests returns the registry digests recorded for a local image.
func (a *Agent) dockerRepoDigests(ctx context.Context, imageID string) ([]string, error) {
	img, err := a.docker.api.ImageInspect(ctx, imageID)
	if err != nil {
		return nil, err
	}
	return img.RepoDigests, nil
}
