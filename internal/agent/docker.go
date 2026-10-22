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

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/smokinstack/sddb/internal/types"
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
	cli       *client.Client
	api       dockerAPI // used for pulls and recreates; cli in production, a fake in tests
	prevMu    sync.Mutex
	prevStats map[string]prevNetStats // keyed by container ID
}

func NewDockerClient() (*DockerClient, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to Docker: %w", err)
	}
	return &DockerClient{
		cli:       cli,
		api:       cli,
		prevStats: make(map[string]prevNetStats),
	}, nil
}

func (d *DockerClient) Close() { d.cli.Close() }

// CollectAll lists all containers and gathers their stats concurrently.
func (d *DockerClient) CollectAll(ctx context.Context) ([]types.ContainerState, error) {
	list, err := d.cli.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	containers := list.Items

	type result struct {
		state types.ContainerState
		err   error
	}
	results := make(chan result, len(containers))

	for _, c := range containers {
		go func(c container.Summary) {
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

	present := make(map[string]bool, len(containers))
	for _, c := range containers {
		present[c.ID] = true
	}
	d.forgetGone(present)
	return states, nil
}

// forgetGone drops rate-calculation samples for containers that no longer
// exist. Every recreated container has a new ID, so without this the map
// grew for as long as the agent ran (noticeably on a busy dev host).
func (d *DockerClient) forgetGone(present map[string]bool) {
	d.prevMu.Lock()
	defer d.prevMu.Unlock()
	for id := range d.prevStats {
		if !present[id] {
			delete(d.prevStats, id)
		}
	}
}

func (d *DockerClient) containerState(ctx context.Context, c container.Summary) (types.ContainerState, error) {
	var name string
	if len(c.Names) > 0 {
		name = strings.TrimPrefix(c.Names[0], "/")
	}

	state := types.ContainerState{
		ID:      c.ID,
		ShortID: c.ID[:12],
		Name:    name,
		Image:   c.Image,
		ImageID: c.ImageID,
		Status:  c.Status,
		State:   string(c.State),
		Created: c.Created,
		Ports:   formatPorts(c.Ports),
		// Compose labels
		ComposeProject: c.Labels["com.docker.compose.project"],
		ComposeService: c.Labels["com.docker.compose.service"],
		ComposeFile:    c.Labels["com.docker.compose.project.config_files"],
		ComposeWorkDir: c.Labels["com.docker.compose.project.working_dir"],
	}

	if c.State == container.StateRunning {
		if err := d.fillStats(ctx, &state); err != nil {
			// non-fatal; leave stats at zero
		}
	}

	// Inspect for restart/exit details — fast local socket call, non-fatal
	if res, err := d.cli.ContainerInspect(ctx, c.ID, client.ContainerInspectOptions{}); err == nil {
		info := res.Container
		state.RestartCount = info.RestartCount
		if info.State != nil {
			state.ExitCode = info.State.ExitCode
			state.OOMKilled = info.State.OOMKilled
			state.FinishedAt = info.State.FinishedAt
		}
	}

	return state, nil
}

func (d *DockerClient) fillStats(ctx context.Context, state *types.ContainerState) error {
	statsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// IncludePreviousSample makes the daemon take two samples a second apart,
	// filling PreCPUStats for the CPU calculation below. (The old client's
	// ContainerStats(…, stream=false) did this implicitly; without it CPU
	// would always read 0%.)
	resp, err := d.cli.ContainerStats(statsCtx, state.ID, client.ContainerStatsOptions{IncludePreviousSample: true})
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
			state.NetRxRate = counterRate(rxBytes, prev.netRxBytes, elapsed)
			state.NetTxRate = counterRate(txBytes, prev.netTxBytes, elapsed)
			state.BlockReadRate = counterRate(blockRead, prev.blockRead, elapsed)
			state.BlockWriteRate = counterRate(blockWrite, prev.blockWrite, elapsed)
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

// counterRate returns the per-second rate between two samples of a cumulative
// counter. Counters reset to zero when a container restarts; subtracting the
// larger previous value from an unsigned counter would wrap around to ~1.8e19
// and show as an absurd spike (also in the host's Net total), so a reset
// reports 0 for that one sample instead.
func counterRate(cur, prev uint64, seconds float64) float64 {
	if cur < prev || seconds <= 0 {
		return 0
	}
	return float64(cur-prev) / seconds
}

// StartContainer starts a stopped container.
func (d *DockerClient) StartContainer(ctx context.Context, id string) error {
	_, err := d.cli.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// StopContainer stops a running container.
func (d *DockerClient) StopContainer(ctx context.Context, id string) error {
	timeout := 10
	_, err := d.cli.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

// RestartContainer restarts a container.
func (d *DockerClient) RestartContainer(ctx context.Context, id string) error {
	timeout := 10
	_, err := d.cli.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &timeout})
	return err
}

// UpgradeContainer pulls the latest image and recreates the container.
// It detects whether the container was started via Compose or bare CLI.
func (d *DockerClient) UpgradeContainer(ctx context.Context, id string) error {
	res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	info := res.Container

	if info.Config.Labels["com.docker.compose.project"] != "" {
		return d.upgradeCompose(ctx, info)
	}
	return upgradeCLI(ctx, d.api, info)
}

// PullImage pulls the latest version of the container's image without
// recreating the container.
func (d *DockerClient) PullImage(ctx context.Context, id string) error {
	res, err := d.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	return pullImage(ctx, d.api, res.Container.Config.Image)
}

func (d *DockerClient) upgradeCompose(ctx context.Context, info container.InspectResponse) error {
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

// PruneImages removes dangling images.
func (d *DockerClient) PruneImages(ctx context.Context) error {
	_, err := d.cli.ImagePrune(ctx, client.ImagePruneOptions{}) // no filters: dangling images only
	return err
}

// GetLogs returns the last `tail` lines of stdout+stderr for a container.
func (d *DockerClient) GetLogs(ctx context.Context, containerID, tail string) (string, error) {
	res, err := d.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect: %w", err)
	}
	info := res.Container

	rc, err := d.cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
	})
	if err != nil {
		return "", fmt.Errorf("logs: %w", err)
	}
	defer rc.Close()

	// Cap the size: tail limits lines, but lines can be arbitrarily long.
	limited := io.LimitReader(rc, types.MaxLogBytes)
	var buf bytes.Buffer
	if info.Config.Tty {
		io.Copy(&buf, limited)
	} else {
		stdcopy.StdCopy(&buf, &buf, limited)
	}
	return buf.String(), nil
}

func formatPorts(ports []container.PortSummary) []string {
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
