package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// dockerAPI is the subset of the Docker client used to pull images and
// recreate containers. *client.Client satisfies it; tests use a fake.
type dockerAPI interface {
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
	ContainerCreate(ctx context.Context, config *container.Config, hostConfig *container.HostConfig, networkingConfig *network.NetworkingConfig, platform *ocispec.Platform, containerName string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, containerID string, options container.StartOptions) error
	ContainerStop(ctx context.Context, containerID string, options container.StopOptions) error
	ContainerRemove(ctx context.Context, containerID string, options container.RemoveOptions) error
	ContainerRename(ctx context.Context, containerID, newContainerName string) error
	NetworkConnect(ctx context.Context, networkID, containerID string, config *network.EndpointSettings) error
	ImagePull(ctx context.Context, refStr string, options image.PullOptions) (io.ReadCloser, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (image.InspectResponse, error)
}

// pullImage pulls ref and waits for the pull to finish. The pull runs as a
// stream: closing it early cancels the pull, and errors (auth, not found,
// network) are reported inside the stream rather than as the call's error.
func pullImage(ctx context.Context, api dockerAPI, ref string) error {
	rc, err := api.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	if err := drainPullStream(rc); err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// drainPullStream reads a Docker progress stream (one JSON object per
// message) to the end and returns the first error message it contains.
func drainPullStream(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&msg); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read pull progress: %w", err)
		}
		if msg.ErrorDetail.Message != "" {
			return errors.New(msg.ErrorDetail.Message)
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
	}
}

// upgradeCLI pulls the container's image and recreates the container with the
// same configuration. The old container is kept (renamed) until the new one
// has started, and is restored if any step fails, so a failed upgrade never
// leaves the service missing.
func upgradeCLI(ctx context.Context, api dockerAPI, info container.InspectResponse) error {
	if info.Config == nil || info.HostConfig == nil {
		return errors.New("container has no config to recreate from")
	}
	imageName := info.Config.Image
	if imageName == "" || strings.HasPrefix(imageName, "sha256:") {
		return fmt.Errorf("container was created from an image ID (%.19s), not a tag; recreate it from a tagged image to enable upgrades", imageName)
	}

	// Image defaults the container inherited, so they can be refreshed from
	// the new image. Best-effort: without them we keep the config as-is.
	var oldDefaults *dockerspec.DockerOCIImageConfig
	if oldImg, err := api.ImageInspect(ctx, info.Image); err == nil {
		oldDefaults = oldImg.Config
	}

	if err := pullImage(ctx, api, imageName); err != nil {
		return err
	}
	newImg, err := api.ImageInspect(ctx, imageName)
	if err != nil {
		return fmt.Errorf("inspect pulled image: %w", err)
	}
	if newImg.ID == info.Image {
		log.Printf("upgrade %s: already running the latest %s", info.Name, imageName)
		return nil
	}

	name := strings.TrimPrefix(info.Name, "/")
	shortID := info.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}

	cfg := *info.Config
	cfg.Image = imageName
	refreshImageDefaults(&cfg, oldDefaults, newImg.Config)
	if cfg.Hostname == shortID {
		cfg.Hostname = "" // Docker's default hostname is the container ID; let the new one get its own
	}
	hostCfg := *info.HostConfig
	hostCfg.Binds = preserveAnonymousVolumes(info.Mounts, info.HostConfig)
	netCfg, extraNets := splitNetworks(info, shortID)
	wasRunning := info.State != nil && info.State.Running

	backup := fmt.Sprintf("%s-sddb-old-%d", name, time.Now().Unix())
	if err := api.ContainerRename(ctx, info.ID, backup); err != nil {
		return fmt.Errorf("rename old container: %w", err)
	}

	oldStopped := false
	rollback := func(newID string, cause error) error {
		// The request context may be what failed, so use a fresh one.
		rctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var errs []error
		if newID != "" {
			if err := api.ContainerRemove(rctx, newID, container.RemoveOptions{Force: true}); err != nil {
				errs = append(errs, fmt.Errorf("remove new container: %w", err))
			}
		}
		if err := api.ContainerRename(rctx, info.ID, name); err != nil {
			errs = append(errs, fmt.Errorf("rename old container back: %w", err))
		}
		if oldStopped {
			if err := api.ContainerStart(rctx, info.ID, container.StartOptions{}); err != nil {
				errs = append(errs, fmt.Errorf("restart old container: %w", err))
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("%w; rollback incomplete (previous container is %q): %w", cause, backup, errors.Join(errs...))
		}
		return fmt.Errorf("%w (rolled back to the previous container)", cause)
	}

	created, err := api.ContainerCreate(ctx, &cfg, &hostCfg, netCfg, nil, name)
	if err != nil {
		return rollback("", fmt.Errorf("create: %w", err))
	}
	for netName, ep := range extraNets {
		if err := api.NetworkConnect(ctx, netName, created.ID, ep); err != nil {
			return rollback(created.ID, fmt.Errorf("connect network %s: %w", netName, err))
		}
	}
	if wasRunning {
		timeout := 10
		// Treat the old container as stopped even if Stop errors part-way, so
		// rollback always tries to bring it back up.
		oldStopped = true
		if err := api.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
			return rollback(created.ID, fmt.Errorf("stop old container: %w", err))
		}
		if err := api.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			return rollback(created.ID, fmt.Errorf("start: %w", err))
		}
	}

	if err := api.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil {
		// The upgrade itself succeeded; just report the leftover.
		log.Printf("upgrade %s: new container is running but the previous one (%s) could not be removed: %v", name, backup, err)
	}
	return nil
}

// refreshImageDefaults updates settings the container inherited from its old
// image (env vars, entrypoint, cmd, workdir, user, labels) to the new image's
// values, while keeping anything the user set explicitly. Without this a
// recreated container keeps the old image's PATH, version variables, etc.
func refreshImageDefaults(cfg *container.Config, oldImg, newImg *dockerspec.DockerOCIImageConfig) {
	if oldImg == nil || newImg == nil {
		return
	}

	// Env: drop entries identical to the old image's, then layer the user's
	// remaining entries over the new image's defaults (Docker merges them).
	var userEnv []string
	for _, e := range cfg.Env {
		if !slices.Contains(oldImg.Env, e) {
			userEnv = append(userEnv, e)
		}
	}
	cfg.Env = userEnv

	if slices.Equal(cfg.Entrypoint, oldImg.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if slices.Equal(cfg.Cmd, oldImg.Cmd) {
		cfg.Cmd = nil
	}
	if cfg.WorkingDir == oldImg.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == oldImg.User {
		cfg.User = ""
	}
	if len(cfg.Labels) > 0 && len(oldImg.Labels) > 0 {
		labels := make(map[string]string, len(cfg.Labels))
		for k, v := range cfg.Labels {
			if ov, ok := oldImg.Labels[k]; ok && ov == v {
				continue // came from the old image; Docker re-adds the new image's labels
			}
			labels[k] = v
		}
		cfg.Labels = labels
	}
}

// preserveAnonymousVolumes returns the container's binds plus its anonymous
// volumes (VOLUME directives with no -v), so the new container reattaches the
// same data instead of starting with fresh, empty volumes.
func preserveAnonymousVolumes(mounts []container.MountPoint, hc *container.HostConfig) []string {
	binds := slices.Clone(hc.Binds)
	covered := make(map[string]bool)
	for _, b := range hc.Binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}
	for _, m := range hc.Mounts {
		covered[m.Target] = true
	}
	for _, m := range mounts {
		if m.Type != mount.TypeVolume || m.Name == "" || covered[m.Destination] {
			continue
		}
		b := m.Name + ":" + m.Destination
		if !m.RW {
			b += ":ro"
		}
		binds = append(binds, b)
		covered[m.Destination] = true
	}
	return binds
}

// splitNetworks returns the networking config for ContainerCreate (only the
// primary network, which every daemon API version accepts) and the remaining
// networks to connect afterwards. Runtime fields such as the old IP address
// and endpoint ID are dropped; user settings (static IPs, aliases, links) kept.
func splitNetworks(info container.InspectResponse, shortID string) (*network.NetworkingConfig, map[string]*network.EndpointSettings) {
	mode := string(info.HostConfig.NetworkMode)
	if mode == "host" || mode == "none" || strings.HasPrefix(mode, "container:") ||
		info.NetworkSettings == nil || len(info.NetworkSettings.Networks) == 0 {
		return nil, nil
	}
	primary := mode
	if primary == "" || primary == "default" {
		primary = "bridge"
	}

	netCfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{}}
	extra := map[string]*network.EndpointSettings{}
	for name, ep := range info.NetworkSettings.Networks {
		if ep == nil {
			continue
		}
		clean := &network.EndpointSettings{
			IPAMConfig: ep.IPAMConfig,
			Links:      ep.Links,
			DriverOpts: ep.DriverOpts,
			GwPriority: ep.GwPriority,
		}
		for _, a := range ep.Aliases {
			if a != shortID { // Docker adds the old short ID as an alias
				clean.Aliases = append(clean.Aliases, a)
			}
		}
		if name == primary {
			netCfg.EndpointsConfig[name] = clean
		} else {
			extra[name] = clean
		}
	}
	return netCfg, extra
}
