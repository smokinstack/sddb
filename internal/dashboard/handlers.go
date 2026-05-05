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

	"github.com/jester/sddb/internal/auth"
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
}

// Config holds everything NewDashboard needs.
type Config struct {
	AgentPort int
	TLS       *tls.Config        // nil = plain HTTP to agents
	Creds     *auth.Credentials  // nil = no login required
	Sessions  *auth.Sessions
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
	}, nil
}

func (d *Dashboard) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", d.handleLogin)
	mux.HandleFunc("/logout", d.handleLogout)
	mux.HandleFunc("/", d.handleIndex)
	mux.HandleFunc("/api/agents", d.handleAgents)
	mux.HandleFunc("/api/agents/", d.handleAgentAction)
	mux.HandleFunc("/api/scan", d.handleScan)
	mux.HandleFunc("/api/command", d.handleCommand)
	mux.HandleFunc("/api/dashboard", d.handleDashboardFragment)
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
	if err := d.tmpl.ExecuteTemplate(w, "dashboard.html", templateData(agents)); err != nil {
		log.Printf("template error: %v", err)
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

	results, err := ScanNetwork(ctx, cidr, d.agentPort, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	enc := json.NewEncoder(w)
	for result := range results {
		enc.Encode(result)
		if canFlush {
			flusher.Flush()
		}
	}
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
			if err := d.tmpl.ExecuteTemplate(&buf, "dashboard.html", templateData(d.state.All())); err != nil {
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
	Agents []*AgentRecord
	Now    time.Time
}

func templateData(agents []*AgentRecord) TemplateAgentData {
	return TemplateAgentData{Agents: agents, Now: time.Now()}
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
