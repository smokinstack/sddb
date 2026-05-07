package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/jester/sddb/internal/types"
)

type prevNetStats struct {
	timestamp  time.Time
	netRxBytes uint64
	netTxBytes uint64
	blockRead  uint64
	blockWrite uint64
}

// DockerClient wraps the Docker SDK client with helpers.
type DockerClient struct {
	cli      *client.Client
	prevMu   sync.Mutex
	prevStats map[string]prevNetStats // keyed by container ID
}

func NewDockerClient() (*DockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to Docker: %w", err)
	}
	return &DockerClient{
		cli:       cli,
		prevStats: make(map[string]prevNetStats),
	}, nil
}

func (d *DockerClient) Close() { d.cli.Close() }

// CollectAll lists all containers and gathers their stats concurrently.
func (d *DockerClient) CollectAll(ctx context.Context) ([]types.ContainerState, error) {
	containers, err := d.cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	type result struct {
		state types.ContainerState
		err   error
	}
	results := make(chan result, len(containers))

	for _, c := range containers {
		go func(c dockertypes.Container) {
			state, err := d.containerState(ctx, c)
			results <- result{state, err}
		}(c)
	}

	states := make([]types.ContainerState, 0, len(containers))
	for range containers {
		r := <-results
		if r.err != nil {
			continue
		}
		states = append(states, r.state)
	}
	return states, nil
}

func (d *DockerClient) containerState(ctx context.Context, c dockertypes.Container) (types.ContainerState, error) {
	name := strings.TrimPrefix(c.Names[0], "/")

	state := types.ContainerState{
		ID:      c.ID,
		ShortID: c.ID[:12],
		Name:    name,
		Image:   c.Image,
		ImageID: c.ImageID,
		Status:  c.Status,
		State:   c.State,
		Created: c.Created,
		Ports:   formatPorts(c.Ports),
		// Compose labels
		ComposeProject: c.Labels["com.docker.compose.project"],
		ComposeService: c.Labels["com.docker.compose.service"],
		ComposeFile:    c.Labels["com.docker.compose.project.config_files"],
		ComposeWorkDir: c.Labels["com.docker.compose.project.working_dir"],
	}

	if c.State == "running" {
		if err := d.fillStats(ctx, &state); err != nil {
			// non-fatal; leave stats at zero
		}
	}

	return state, nil
}

func (d *DockerClient) fillStats(ctx context.Context, state *types.ContainerState) error {
	statsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := d.cli.ContainerStats(statsCtx, state.ID, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var s container.StatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return err
	}

	// CPU
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage - s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage - s.PreCPUStats.SystemUsage)
	numCPU := float64(s.CPUStats.OnlineCPUs)
	if numCPU == 0 {
		numCPU = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && numCPU > 0 {
		state.CPUPercent = (cpuDelta / sysDelta) * numCPU * 100.0
	}

	// Memory (subtract page cache for accurate RSS)
	memUsage := s.MemoryStats.Usage
	if cache, ok := s.MemoryStats.Stats["inactive_file"]; ok && cache < memUsage {
		memUsage -= cache
	}
	state.MemUsage = memUsage
	state.MemLimit = s.MemoryStats.Limit
	if s.MemoryStats.Limit > 0 {
		state.MemPercent = float64(memUsage) / float64(s.MemoryStats.Limit) * 100.0
	}

	// Network totals
	var rxBytes, txBytes uint64
	for _, n := range s.Networks {
		rxBytes += n.RxBytes
		txBytes += n.TxBytes
	}
	state.NetRxTotal = rxBytes
	state.NetTxTotal = txBytes

	// Block IO totals
	var blockRead, blockWrite uint64
	for _, bio := range s.BlkioStats.IoServiceBytesRecursive {
		switch bio.Op {
		case "Read":
			blockRead += bio.Value
		case "Write":
			blockWrite += bio.Value
		}
	}

	// Compute rates from previous sample
	now := time.Now()
	d.prevMu.Lock()
	if prev, ok := d.prevStats[state.ID]; ok {
		elapsed := now.Sub(prev.timestamp).Seconds()
		if elapsed > 0 {
			state.NetRxRate = float64(rxBytes-prev.netRxBytes) / elapsed
			state.NetTxRate = float64(txBytes-prev.netTxBytes) / elapsed
			state.BlockReadRate = float64(blockRead-prev.blockRead) / elapsed
			state.BlockWriteRate = float64(blockWrite-prev.blockWrite) / elapsed
		}
	}
	d.prevStats[state.ID] = prevNetStats{
		timestamp:  now,
		netRxBytes: rxBytes,
		netTxBytes: txBytes,
		blockRead:  blockRead,
		blockWrite: blockWrite,
	}
	d.prevMu.Unlock()

	return nil
}

// StartContainer starts a stopped container.
func (d *DockerClient) StartContainer(ctx context.Context, id string) error {
	return d.cli.ContainerStart(ctx, id, container.StartOptions{})
}

// StopContainer stops a running container.
func (d *DockerClient) StopContainer(ctx context.Context, id string) error {
	timeout := 10
	return d.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

// RestartContainer restarts a container.
func (d *DockerClient) RestartContainer(ctx context.Context, id string) error {
	timeout := 10
	return d.cli.ContainerRestart(ctx, id, container.StopOptions{Timeout: &timeout})
}

// UpgradeContainer pulls the latest image and recreates the container.
// It detects whether the container was started via Compose or bare CLI.
func (d *DockerClient) UpgradeContainer(ctx context.Context, id string) error {
	info, err := d.cli.ContainerInspect(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}

	if info.Config.Labels["com.docker.compose.project"] != "" {
		return d.upgradeCompose(ctx, info)
	}
	return d.upgradeCLI(ctx, info)
}

func (d *DockerClient) upgradeCompose(ctx context.Context, info dockertypes.ContainerJSON) error {
	composeFile := info.Config.Labels["com.docker.compose.project.config_files"]
	service := info.Config.Labels["com.docker.compose.service"]
	workDir := info.Config.Labels["com.docker.compose.project.working_dir"]
	if workDir == "" {
		workDir = "/"
	}
	if err := runCompose(ctx, workDir, composeFile, "pull", service); err != nil {
		return fmt.Errorf("compose pull: %w", err)
	}
	return runCompose(ctx, workDir, composeFile, "up", "-d", service)
}

func (d *DockerClient) upgradeCLI(ctx context.Context, info dockertypes.ContainerJSON) error {
	imageName := info.Config.Image

	// Pull new image
	reader, err := d.cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull image: %w", err)
	}
	io.Copy(io.Discard, reader)
	reader.Close()

	// Stop and remove the old container
	timeout := 10
	if err := d.cli.ContainerStop(ctx, info.ID, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := d.cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("remove: %w", err)
	}

	// Recreate using the original image name (tag), not the sha256 ID, so
	// that future update checks and pulls continue to work against the registry.
	// Docker inspect returns names with a leading "/"; strip it for ContainerCreate.
	cfg := info.Config
	cfg.Image = imageName

	hostCfg := info.HostConfig
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: info.NetworkSettings.Networks,
	}

	name := strings.TrimPrefix(info.Name, "/")
	created, err := d.cli.ContainerCreate(ctx, cfg, hostCfg, netCfg, nil, name)
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}

	return d.cli.ContainerStart(ctx, created.ID, container.StartOptions{})
}

// PruneImages removes dangling images.
func (d *DockerClient) PruneImages(ctx context.Context) error {
	_, err := d.cli.ImagesPrune(ctx, filters.Args{})
	return err
}

// GetLogs returns the last `tail` lines of stdout+stderr for a container.
func (d *DockerClient) GetLogs(ctx context.Context, containerID, tail string) (string, error) {
	info, err := d.cli.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", fmt.Errorf("inspect: %w", err)
	}

	rc, err := d.cli.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
	})
	if err != nil {
		return "", fmt.Errorf("logs: %w", err)
	}
	defer rc.Close()

	var buf bytes.Buffer
	if info.Config.Tty {
		io.Copy(&buf, rc)
	} else {
		stdcopy.StdCopy(&buf, &buf, rc)
	}
	return buf.String(), nil
}

func formatPorts(ports []dockertypes.Port) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, p := range ports {
		var s string
		if p.PublicPort != 0 {
			s = fmt.Sprintf("%d→%d/%s", p.PublicPort, p.PrivatePort, p.Type)
		} else {
			s = fmt.Sprintf("%d/%s", p.PrivatePort, p.Type)
		}
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			result = append(result, s)
		}
	}
	return result
}
