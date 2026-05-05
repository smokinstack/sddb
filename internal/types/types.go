package types

// AgentInfo is returned by the agent's /api/info endpoint.
type AgentInfo struct {
	ID             string `json:"id"`
	Hostname       string `json:"hostname"`
	UpdateInterval int    `json:"update_interval"` // seconds
	Version        string `json:"version"`
}

// ContainerState is the full state + stats snapshot for one container.
type ContainerState struct {
	ID      string `json:"id"`
	ShortID string `json:"short_id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	Status  string `json:"status"` // human string e.g. "Up 2 hours"
	State   string `json:"state"`  // running, exited, paused, restarting, created, dead

	// CPU & memory
	CPUPercent float64 `json:"cpu_percent"`
	MemUsage   uint64  `json:"mem_usage"`  // bytes
	MemLimit   uint64  `json:"mem_limit"`  // bytes
	MemPercent float64 `json:"mem_percent"`

	// Network rates (bytes/sec computed by agent)
	NetRxRate float64 `json:"net_rx_rate"`
	NetTxRate float64 `json:"net_tx_rate"`
	// Network totals
	NetRxTotal uint64 `json:"net_rx_total"`
	NetTxTotal uint64 `json:"net_tx_total"`

	// Block IO rates (bytes/sec)
	BlockReadRate  float64 `json:"block_read_rate"`
	BlockWriteRate float64 `json:"block_write_rate"`

	// Compose metadata (empty if started via CLI)
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	ComposeFile    string `json:"compose_file,omitempty"`
	ComposeWorkDir string `json:"compose_work_dir,omitempty"`

	UpdateAvailable bool     `json:"update_available"`
	Created         int64    `json:"created"` // unix timestamp
	Ports           []string `json:"ports"`
}

// IsCompose returns true when this container was started by Docker Compose.
func (c *ContainerState) IsCompose() bool {
	return c.ComposeProject != ""
}

// StatsResponse is the payload returned by GET /api/containers.
type StatsResponse struct {
	Agent      AgentInfo        `json:"agent"`
	Containers []ContainerState `json:"containers"`
	Timestamp  int64            `json:"timestamp"`
}

// CommandRequest is sent to POST /api/containers/{id}/{action}.
type CommandRequest struct {
	// Action is one of: start, stop, restart, upgrade
	Action      string `json:"action"`
	ContainerID string `json:"container_id"`
}

// CommandResponse is returned after executing a command.
type CommandResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}
