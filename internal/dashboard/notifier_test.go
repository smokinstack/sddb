package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/smokinstack/sddb/internal/config"
	"github.com/smokinstack/sddb/internal/types"
)

func testNotifier(t *testing.T) (*Notifier, *[]string, *time.Time) {
	t.Helper()
	cfg, err := config.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Update(func(c *config.Config) { c.NtfyURL = "https://ntfy.example/topic" })
	n := newNotifier(cfg)
	now := time.Unix(1_700_000_000, 0)
	n.now = func() time.Time { return now }
	var sent []string
	n.deliver = func(url, title, message, priority string) { sent = append(sent, title) }
	return n, &sent, &now
}

// stopWith reports a running container, then the same container exited.
func stopWith(n *Notifier, name string, exitCode int, oom bool) {
	n.Check("h:8484", []types.ContainerState{{Name: name, State: "running"}})
	n.Check("h:8484", []types.ContainerState{{Name: name, State: "exited", ExitCode: exitCode, OOMKilled: oom}})
}

func TestNoCrashAlertForIntentionalStops(t *testing.T) {
	cases := []struct {
		name      string
		exit      int
		oom       bool
		dashboard bool // stopped from the dashboard just before
		alert     string
	}{
		{"clean exit", 0, false, false, ""},
		{"docker stop, SIGTERM (143)", 143, false, false, ""},
		{"dashboard Stop, killed after timeout (137)", 137, false, true, ""},
		{"killed (137) with no dashboard action", 137, false, false, "Crashed"},
		{"real crash", 1, false, false, "Crashed"},
		{"real crash right after a dashboard restart", 1, false, true, "Crashed"},
		{"OOM kill is always reported", 137, true, true, "OOMKilled"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, sent, _ := testNotifier(t)
			if c.dashboard {
				n.ExpectStop("h:8484", "web")
			}
			stopWith(n, "web", c.exit, c.oom)
			got := strings.Join(*sent, ",")
			if c.alert == "" && got != "" {
				t.Fatalf("unexpected alert %q", got)
			}
			if c.alert != "" && !strings.HasPrefix(got, c.alert) {
				t.Fatalf("alerts = %q, want one starting %q", got, c.alert)
			}
		})
	}
}

func TestExpectedStopExpires(t *testing.T) {
	n, sent, now := testNotifier(t)
	n.ExpectStop("h:8484", "web")
	*now = now.Add(expectedStopWindow + time.Second)
	stopWith(n, "web", 137, false)
	if len(*sent) != 1 {
		t.Fatalf("a kill long after a dashboard stop should alert, got %v", *sent)
	}
}
