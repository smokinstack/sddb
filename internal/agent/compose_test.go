package agent

import (
	"errors"
	"os/exec"
	"slices"
	"testing"
)

func TestComposeFileArgs(t *testing.T) {
	cases := map[string][]string{
		"":                                       nil,
		"/srv/app/docker-compose.yml":            {"-f", "/srv/app/docker-compose.yml"},
		"/srv/app/compose.yml,/srv/app/prod.yml": {"-f", "/srv/app/compose.yml", "-f", "/srv/app/prod.yml"},
		" /a.yml , /b.yml ,":                     {"-f", "/a.yml", "-f", "/b.yml"},
	}
	for label, want := range cases {
		if got := composeFileArgs(label); !slices.Equal(got, want) {
			t.Errorf("composeFileArgs(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestComposePluginMissing(t *testing.T) {
	exitErr := errors.New("exit status 1")
	cases := []struct {
		name   string
		err    error
		stderr string
		want   bool
	}{
		{"docker not installed", &exec.Error{Name: "docker", Err: exec.ErrNotFound}, "", true},
		{"plugin missing", exitErr, "docker: 'compose' is not a docker command.", true},
		{"plugin missing (Docker 28+)", exitErr, "docker: unknown command: docker compose\n\nRun 'docker --help' for more information", true},
		{"real compose failure", exitErr, "open /home/jester/pprism/docker-compose.yml: no such file or directory", false},
		{"pull failure", exitErr, "Error response from daemon: toomanyrequests", false},
	}
	for _, c := range cases {
		if got := composePluginMissing(c.err, c.stderr); got != c.want {
			t.Errorf("%s: composePluginMissing = %v, want %v", c.name, got, c.want)
		}
	}
}
