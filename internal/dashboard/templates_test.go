package dashboard

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

// Renders the real templates so template errors fail in CI, not in the browser.
func TestDashboardTemplateShowsJobStatus(t *testing.T) {
	tmpl, err := loadTemplates(os.DirFS("../../cmd/dashboard/web"))
	if err != nil {
		t.Fatal(err)
	}

	agent := &AgentRecord{ID: "a1", Addr: "h:8484", Online: true, LastSeen: time.Now(),
		LastStats: types.StatsResponse{
			Agent: types.AgentInfo{Hostname: "plexmon", Version: "v0.1.0-42-gabc1234"},
			Host:  types.HostStats{MemTotal: 1},
			Containers: []types.ContainerState{
				{ID: "1", ShortID: "1", Name: "upgrading", State: "running", UpdateAvailable: true},
				{ID: "2", ShortID: "2", Name: "broken", State: "running", UpdateAvailable: true},
				{ID: "3", ShortID: "3", Name: "plain", State: "running"},
				{ID: "4", ShortID: "4", Name: "stopped", State: "exited"},
			},
			Jobs: map[string]types.JobStatus{
				"upgrading": {Action: "upgrade", State: types.JobRunning},
				"broken":    {Action: "upgrade", State: types.JobFailed, Error: "compose pull: unauthorized"},
			},
		}}
	noJobs := &AgentRecord{ID: "a2", Addr: "h2:8484", Online: true,
		LastStats: types.StatsResponse{Host: types.HostStats{MemTotal: 1},
			Containers: []types.ContainerState{{ID: "5", ShortID: "5", Name: "x", State: "running", UpdateAvailable: true}}}}

	var buf bytes.Buffer
	data := TemplateAgentData{Agents: []*AgentRecord{agent, noJobs}, Now: time.Now()}
	if err := tmpl.ExecuteTemplate(&buf, "dashboard.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"Upgrading…", "Upgrade failed: compose pull: unauthorized", ">v0.1.0-42-gabc1234</span>"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered dashboard missing %q", want)
		}
	}
	// The Upgrade button is hidden while an upgrade runs: 3 containers have
	// updates, one of which is mid-upgrade.
	if n := strings.Count(out, "','upgrade')"); n != 2 {
		t.Errorf("found %d Upgrade buttons, want 2", n)
	}
}

// The page can stop and upgrade containers, so it must not run scripts or
// load styles from third-party CDNs; everything is served from /static.
func TestTemplatesLoadNoThirdPartyScripts(t *testing.T) {
	files, err := filepath.Glob("../../cmd/dashboard/web/templates/*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("no templates found: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{`<script src="http`, `<script src="//`, `<link rel="stylesheet" href="http`} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s loads an external resource (%s…)", filepath.Base(f), bad)
			}
		}
	}
	for _, asset := range []string{"app.css", "htmx.min.js"} {
		if _, err := os.Stat("../../cmd/dashboard/web/static/" + asset); err != nil {
			t.Errorf("missing static asset %s: %v", asset, err)
		}
	}
}

func TestProtectedContainersToggleAndRender(t *testing.T) {
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := NewState("")
	d, err := NewDashboard(state, NewPoller(state, time.Hour, nil, cfg), os.DirFS("../../cmd/dashboard/web"), Config{Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}

	toggle := func() {
		rec := httptest.NewRecorder()
		d.handleConfig(rec, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"toggle_protected":"db"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("toggle: status %d %s", rec.Code, rec.Body.String())
		}
	}
	render := func() string {
		agent := &AgentRecord{ID: "a", Addr: "h:8484", Online: true, LastStats: types.StatsResponse{
			Host: types.HostStats{MemTotal: 1},
			Containers: []types.ContainerState{
				{ID: "db-id", ShortID: "db-id", Name: "db", State: "running", UpdateAvailable: true},
				{ID: "web-id", ShortID: "web-id", Name: "web", State: "running"},
			}}}
		var buf bytes.Buffer
		if err := d.tmpl.ExecuteTemplate(&buf, "dashboard.html", d.templateData([]*AgentRecord{agent})); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	toggle()
	if !cfg.IsProtected("db") {
		t.Fatal("toggle_protected did not protect db")
	}
	out := render()
	if strings.Contains(out, "'db-id','stop')") || strings.Contains(out, "'db-id','upgrade')") {
		t.Error("protected container still shows Stop/Upgrade")
	}
	if !strings.Contains(out, "'web-id','stop')") {
		t.Error("unprotected container lost its Stop button")
	}
	if !strings.Contains(out, "Protected: start, stop, restart and upgrade are disabled") {
		t.Error("Lock button doesn't show the protected state")
	}

	toggle()
	if cfg.IsProtected("db") || !strings.Contains(render(), "'db-id','stop')") {
		t.Fatal("second toggle did not unprotect db")
	}
}
