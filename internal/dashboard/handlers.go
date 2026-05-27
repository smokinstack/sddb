package dashboard

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jester/sddb/internal/ai"
	"github.com/jester/sddb/internal/auth"
	"github.com/jester/sddb/internal/config"
	"github.com/jester/sddb/internal/types"
)

// Dashboard wires together state, poller, and HTTP handlers.
type Dashboard struct {
	state     *State
	poller    *Poller
	notify    chan struct{}
	tmpl      *template.Template
	webFS     fs.FS
	agentPort int
	tlsConfig *tls.Config // nil = plain HTTP to agents
	creds     *auth.Credentials
	sessions  *auth.Sessions
	ai        *ai.Client
	cfg       *config.Store
}

// Config holds everything NewDashboard needs.
type Config struct {
	AgentPort int
	TLS       *tls.Config       // nil = plain HTTP to agents
	Creds     *auth.Credentials // nil = no login required
	Sessions  *auth.Sessions
	AI        *ai.Client
	Cfg       *config.Store
}

func NewDashboard(state *State, poller *Poller, notify chan struct{}, webFS fs.FS, cfg Config) (*Dashboard, error) {
	tmpl, err := loadTemplates(webFS)
	if err != nil {
		return nil, err
	}
	return &Dashboard{
		state:     state,
		poller:    poller,
		notify:    notify,
		tmpl:      tmpl,
		webFS:     webFS,
		agentPort: cfg.AgentPort,
		tlsConfig: cfg.TLS,
		creds:     cfg.Creds,
		sessions:  cfg.Sessions,
		ai:        cfg.AI,
		cfg:       cfg.Cfg,
	}, nil
}

func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", d.handleLogin)
	mux.HandleFunc("/logout", d.handleLogout)

	// Static assets — served without auth so browsers can fetch favicon etc.
	staticFS, err := fs.Sub(d.webFS, "static")
	if err == nil {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	}

	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/agents", d.handleAgents)
	mux.HandleFunc("/api/agents/", d.handleAgentAction)
	mux.HandleFunc("/api/scan", d.handleScan)
	mux.HandleFunc("/api/command", d.handleCommand)
	mux.HandleFunc("/api/dashboard", d.handleDashboardFragment)
	mux.HandleFunc("/api/sidebar", d.handleSidebarFragment)
	mux.HandleFunc("/api/main", d.handleMainFragment)
	mux.HandleFunc("/api/logs", d.handleLogs)
	mux.HandleFunc("/api/ai", d.handleAI)
	mux.HandleFunc("/api/config", d.handleConfig)
	mux.HandleFunc("/events", d.handleSSE)

	if d.creds != nil {
		return d.requireAuth(mux)
	}
	return mux
}

// requireAuth wraps the mux with session-cookie authentication.
func (d *Dashboard) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(auth.SessionCookie)
		if err != nil || !d.sessions.Valid(cookie.Value) {
			if r.Header.Get("HX-Request") == "true" {
				// Tell HTMX to do a full page redirect rather than swap the login HTML into a panel
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d *Dashboard) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		username := r.FormValue("username")
		password := r.FormValue("password")
		if auth.Verify(d.creds, username, password) {
			token := d.sessions.Create()
			http.SetCookie(w, &http.Cookie{
				Name:     auth.SessionCookie,
				Value:    token,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
				MaxAge:   int(24 * time.Hour / time.Second),
			})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		d.tmpl.ExecuteTemplate(w, "login.html", map[string]string{"Error": "Invalid username or password"})
		return
	}
	d.tmpl.ExecuteTemplate(w, "login.html", nil)
}

func (d *Dashboard) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.SessionCookie); err == nil {
		d.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:   auth.SessionCookie,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleIndex serves the full HTML page.
func (d *Dashboard) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	d.tmpl.ExecuteTemplate(w, "index.html", map[string]bool{
		"AuthEnabled": d.creds != nil,
	})
}

// handleDashboardFragment returns the rendered agent panels for HTMX polling.
func (d *Dashboard) handleDashboardFragment(w http.ResponseWriter, r *http.Request) {
	agents := d.state.All()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "dashboard.html", d.templateData(agents)); err != nil {
		log.Printf("template error: %v", err)
	}
}

func (d *Dashboard) handleSidebarFragment(w http.ResponseWriter, r *http.Request) {
	agents := d.state.All()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "sidebar.html", d.templateData(agents)); err != nil {
		log.Printf("sidebar template error: %v", err)
	}
}

func (d *Dashboard) handleLogs(w http.ResponseWriter, r *http.Request) {
	agentAddr := r.URL.Query().Get("agent")
	containerID := r.URL.Query().Get("container")
	tail := r.URL.Query().Get("tail")
	if agentAddr == "" || containerID == "" {
		http.Error(w, "agent and container required", http.StatusBadRequest)
		return
	}
	if tail == "" {
		tail = "100"
	}

	scheme := "http"
	var transport http.RoundTripper
	if d.tlsConfig != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: d.tlsConfig}
	}

	url := fmt.Sprintf("%s://%s/api/containers/%s/logs?tail=%s", scheme, agentAddr, containerID, tail)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, url, nil)
	client := &http.Client{Timeout: 30 * time.Second, Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.Copy(w, resp.Body)
}

func (d *Dashboard) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := d.cfg.Get()
		var providers []string
		if d.ai != nil {
			providers = d.ai.AvailableProviders()
		}
		writeJSON(w, map[string]any{
			"ai_provider":         cfg.AIProvider,
			"auto_update":         cfg.AutoUpdate,
			"available_providers": providers,
		})

	case http.MethodPatch:
		var req struct {
			AIProvider       *string `json:"ai_provider"`
			ToggleAutoUpdate *struct {
				Addr string `json:"addr"`
				Name string `json:"name"`
			} `json:"toggle_auto_update"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := d.cfg.Update(func(c *config.Config) {
			if req.AIProvider != nil {
				c.AIProvider = *req.AIProvider
			}
			if req.ToggleAutoUpdate != nil {
				key := req.ToggleAutoUpdate.Addr + "::" + req.ToggleAutoUpdate.Name
				c.AutoUpdate[key] = !c.AutoUpdate[key]
			}
		}); err != nil {
			http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		cfg := d.cfg.Get()
		var providers []string
		if d.ai != nil {
			providers = d.ai.AvailableProviders()
		}
		writeJSON(w, map[string]any{
			"ai_provider":         cfg.AIProvider,
			"auto_update":         cfg.AutoUpdate,
			"available_providers": providers,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (d *Dashboard) handleAI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.ai == nil || !d.ai.Available() {
		http.Error(w, "AI not configured — set ANTHROPIC_API_KEY or OPENAI_API_KEY", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Type          string `json:"type"` // "logs" or "health"
		ContainerName string `json:"container_name"`
		Image         string `json:"image"`
		Content       string `json:"content"`    // logs text (logs type)
		AgentAddr     string `json:"agent_addr"` // health type
		ContainerID   string `json:"container_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var prompt string
	switch req.Type {
	case "logs":
		prompt = fmt.Sprintf(
			"Analyze these Docker container logs and report:\n"+
				"1. Any errors or exceptions\n"+
				"2. Any warnings\n"+
				"3. Anything unusual or worth investigating\n"+
				"Be direct and concise. Do not ask for more information.\n\n"+
				"Container: %s\nImage: %s\n\nLOGS (last 150 lines):\n%s",
			req.ContainerName, req.Image, tailLines(req.Content, 150))

	case "health":
		var cs *types.ContainerState
		for _, agent := range d.state.All() {
			if agent.Addr == req.AgentAddr {
				for i := range agent.LastStats.Containers {
					c := &agent.LastStats.Containers[i]
					if c.ID == req.ContainerID || c.ShortID == req.ContainerID {
						cs = c
						break
					}
				}
			}
		}
		if cs == nil {
			http.Error(w, "container not found", http.StatusNotFound)
			return
		}
		ports := strings.Join(cs.Ports, ", ")
		if ports == "" {
			ports = "none"
		}
		compose := "standalone"
		if cs.ComposeProject != "" {
			compose = fmt.Sprintf("project=%s service=%s", cs.ComposeProject, cs.ComposeService)
		}
		prompt = fmt.Sprintf(
			"You are a DevOps and security assistant. Analyze this Docker container and provide:\n"+
				"1. Security concerns (exposed ports, running as root, capabilities, etc.)\n"+
				"2. Configuration recommendations\n"+
				"3. Health or stability issues\n"+
				"4. General observations\n\n"+
				"Be concise and actionable.\n\n"+
				"Container: %s\nImage: %s\nState: %s\nUpdate available: %v\n"+
				"Ports: %s\nCompose: %s\nCPU: %.1f%%\nMemory: %s / %s",
			cs.Name, cs.Image, cs.State, cs.UpdateAvailable,
			ports, compose, cs.CPUPercent,
			formatBytes(cs.MemUsage), formatBytes(cs.MemLimit))

	default:
		http.Error(w, "unknown type", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	provider := d.cfg.Get().AIProvider
	result, err := d.ai.AskWithProvider(ctx, prompt, provider)
	if err != nil {
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"result": result})
}

func (d *Dashboard) handleMainFragment(w http.ResponseWriter, r *http.Request) {
	agents := d.state.All()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := d.tmpl.ExecuteTemplate(w, "main.html", d.templateData(agents)); err != nil {
		log.Printf("main template error: %v", err)
	}
}

// handleAgents: GET lists agents, POST adds an agent.
func (d *Dashboard) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agents := d.state.All()
		writeJSON(w, agents)

	case http.MethodPost:
		var body struct {
			Addr  string `json:"addr"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.Addr == "" {
			http.Error(w, "addr required", http.StatusBadRequest)
			return
		}
		// Ensure port is specified
		if !strings.Contains(body.Addr, ":") {
			body.Addr = fmt.Sprintf("%s:%d", body.Addr, d.agentPort)
		}
		d.state.AddAgent(body.Addr, body.Label)
		// Trigger immediate poll
		go d.poller.PollNow(context.Background(), body.Addr)
		writeJSON(w, map[string]string{"status": "added", "addr": body.Addr})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAgentAction handles DELETE /api/agents/{id}
func (d *Dashboard) handleAgentAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if d.state.RemoveAgent(id) {
		writeJSON(w, map[string]bool{"removed": true})
	} else {
		http.NotFound(w, r)
	}
}

// handleScan scans the network for agents and streams results as JSON-lines.
func (d *Dashboard) handleScan(w http.ResponseWriter, r *http.Request) {
	cidr := r.URL.Query().Get("cidr")
	if cidr == "" {
		var err error
		cidr, err = LocalCIDR()
		if err != nil {
			http.Error(w, "cannot detect local network: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, canFlush := w.(http.Flusher)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	results, err := ScanNetwork(ctx, cidr, d.agentPort, 50, d.tlsConfig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Build set of already-added online agents so scan can skip them
	knownOnline := make(map[string]bool)
	for _, a := range d.state.All() {
		if a.Online {
			knownOnline[a.Addr] = true
		}
	}

	enc := json.NewEncoder(w)
	for result := range results {
		if knownOnline[result.Addr] {
			continue
		}
		enc.Encode(result)
		if canFlush {
			flusher.Flush()
		}
	}
}

var allowedActions = map[string]bool{
	"start": true, "stop": true, "restart": true, "upgrade": true, "pull": true,
}

// handleCommand forwards a command to the appropriate agent.
// POST /api/command  body: {agent_addr, container_id, action}
func (d *Dashboard) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		AgentAddr   string `json:"agent_addr"`
		ContainerID string `json:"container_id"`
		Action      string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Validate action against allowlist.
	if !allowedActions[req.Action] {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}

	// Validate agent_addr is a registered agent to prevent SSRF.
	if _, ok := d.state.findByAddr(req.AgentAddr); !ok {
		http.Error(w, "unknown agent", http.StatusBadRequest)
		return
	}

	scheme := "http"
	var transport http.RoundTripper
	if d.tlsConfig != nil {
		scheme = "https"
		transport = &http.Transport{TLSClientConfig: d.tlsConfig}
	}

	url := fmt.Sprintf("%s://%s/api/containers/%s/%s", scheme, req.AgentAddr, req.ContainerID, req.Action)
	agentReq, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, nil)

	client := &http.Client{Timeout: 90 * time.Second, Transport: transport}
	resp, err := client.Do(agentReq)
	if err != nil {
		writeJSON(w, types.CommandResponse{Success: false, Message: err.Error()})
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)

	// Trigger a stats refresh after a brief delay
	go func() {
		time.Sleep(time.Second)
		d.poller.PollNow(context.Background(), req.AgentAddr)
	}()
}

// handleSSE pushes a "refresh" event to connected browsers whenever new stats arrive.
func (d *Dashboard) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send an initial ping
	fmt.Fprintf(w, "event: connected\ndata: ok\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-d.notify:
			// Render the dashboard fragment and send it as an SSE event
			var buf bytes.Buffer
			if err := d.tmpl.ExecuteTemplate(&buf, "dashboard.html", d.templateData(d.state.All())); err != nil {
				log.Printf("SSE template error: %v", err)
				continue
			}
			// SSE data cannot contain raw newlines — encode as a series of "data:" lines
			lines := strings.Split(buf.String(), "\n")
			for _, line := range lines {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			fmt.Fprintf(w, "\n")
			flusher.Flush()
		}
	}
}

// TemplateAgentData is passed to dashboard templates.
type TemplateAgentData struct {
	Agents     []*AgentRecord
	Now        time.Time
	AutoUpdate map[string]bool // "agentAddr::containerName" → enabled
}

func (d *Dashboard) templateData(agents []*AgentRecord) TemplateAgentData {
	var au map[string]bool
	if d.cfg != nil {
		au = d.cfg.Get().AutoUpdate
	}
	return TemplateAgentData{Agents: agents, Now: time.Now(), AutoUpdate: au}
}

func loadTemplates(webFS fs.FS) (*template.Template, error) {
	funcMap := template.FuncMap{
		"formatBytes":   formatBytes,
		"formatRate":    formatRate,
		"formatPercent": formatPercent,
		"stateClass":    stateClass,
		"stateBadge":    stateBadge,
		"since":         since,
		"add":           func(a, b int) int { return a + b },
		"itoa":          strconv.Itoa,
		"cpuColor":          cpuColor,
		"memColor":          memColor,
		"clamp":             clamp,
		"runningContainers": func(cs []types.ContainerState) []types.ContainerState { return filteredSorted(cs, true) },
		"stoppedContainers": func(cs []types.ContainerState) []types.ContainerState { return filteredSorted(cs, false) },
		"diskPercent": func(used, total uint64) float64 {
			if total == 0 {
				return 0
			}
			return float64(used) / float64(total) * 100
		},
		"cpuTextColor": func(pct float64) string {
			switch {
			case pct >= 80:
				return "text-red-400"
			case pct >= 50:
				return "text-yellow-400"
			default:
				return "text-slate-200"
			}
		},
		"diskTextColor": func(pct float64) string {
			switch {
			case pct >= 90:
				return "text-red-400"
			case pct >= 75:
				return "text-yellow-400"
			default:
				return "text-slate-200"
			}
		},
		"updatesAvailable": func(cs []types.ContainerState) int {
			n := 0
			for _, c := range cs {
				if c.UpdateAvailable {
					n++
				}
			}
			return n
		},
		// dict builds a map[string]any from key-value pairs (for passing to sub-templates)
		"dict": func(pairs ...any) (map[string]any, error) {
			if len(pairs)%2 != 0 {
				return nil, fmt.Errorf("dict: odd number of arguments")
			}
			m := make(map[string]any, len(pairs)/2)
			for i := 0; i < len(pairs); i += 2 {
				k, ok := pairs[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key must be string")
				}
				m[k] = pairs[i+1]
			}
			return m, nil
		},
	}
	return template.New("").Funcs(funcMap).ParseFS(webFS, "templates/*.html")
}

func filteredSorted(cs []types.ContainerState, running bool) []types.ContainerState {
	var out []types.ContainerState
	for _, c := range cs {
		if (c.State == "running") == running {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// Template helper functions

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func formatRate(bps float64) string {
	if bps < 1 {
		return "0 B/s"
	}
	return formatBytes(uint64(bps)) + "/s"
}

func formatPercent(f float64) string {
	return fmt.Sprintf("%.1f%%", f)
}

func stateClass(state string) string {
	switch state {
	case "running":
		return "text-green-400"
	case "exited", "dead":
		return "text-red-400"
	case "paused":
		return "text-yellow-400"
	case "restarting":
		return "text-orange-400"
	default:
		return "text-gray-400"
	}
}

func stateBadge(state string) string {
	switch state {
	case "running":
		return "bg-green-500/20 text-green-400 border-green-500/30"
	case "exited", "dead":
		return "bg-red-500/20 text-red-400 border-red-500/30"
	case "paused":
		return "bg-yellow-500/20 text-yellow-400 border-yellow-500/30"
	case "restarting":
		return "bg-orange-500/20 text-orange-400 border-orange-500/30"
	default:
		return "bg-gray-500/20 text-gray-400 border-gray-500/30"
	}
}

func cpuColor(pct float64) string {
	switch {
	case pct >= 80:
		return "bg-red-500"
	case pct >= 50:
		return "bg-yellow-500"
	default:
		return "bg-blue-500"
	}
}

func memColor(pct float64) string {
	switch {
	case pct >= 90:
		return "bg-red-500"
	case pct >= 70:
		return "bg-yellow-500"
	default:
		return "bg-emerald-500"
	}
}

func clamp(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func since(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
