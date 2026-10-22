package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// composeFileArgs turns the com.docker.compose.project.config_files label into
// -f arguments. Compose records every file of a project (e.g. a base file plus
// an override) in that one label, separated by commas; passing the label as a
// single -f argument made multi-file projects fail to upgrade.
func composeFileArgs(configFilesLabel string) []string {
	var args []string
	for _, f := range strings.Split(configFilesLabel, ",") {
		if f = strings.TrimSpace(f); f != "" {
			args = append(args, "-f", f)
		}
	}
	return args
}

// runCompose runs `docker compose -f <file>... <args...>` in the given working
// directory. It falls back to the legacy `docker-compose` binary only when the
// compose plugin itself is missing; a real failure (pull error, bad file) is
// reported as-is instead of being retried with a different tool.
func runCompose(ctx context.Context, workDir, configFilesLabel string, args ...string) error {
	fileArgs := composeFileArgs(configFilesLabel)

	stderr, err := runIn(ctx, workDir, "docker", append(append([]string{"compose"}, fileArgs...), args...)...)
	if err == nil {
		return nil
	}
	if !composePluginMissing(err, stderr) {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr))
	}

	legacyStderr, legacyErr := runIn(ctx, workDir, "docker-compose", append(fileArgs, args...)...)
	if legacyErr != nil {
		return fmt.Errorf("docker compose plugin not available, and docker-compose failed: %w: %s",
			legacyErr, strings.TrimSpace(legacyStderr))
	}
	return nil
}

func runIn(ctx context.Context, dir, name string, args ...string) (stderr string, err error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var buf bytes.Buffer
	cmd.Stderr = &buf
	err = cmd.Run()
	return buf.String(), err
}

// composePluginMissing reports whether `docker compose` failed because the
// compose plugin (or docker itself) isn't installed, as opposed to compose
// running and failing.
func composePluginMissing(err error, stderr string) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true // docker binary not found
	}
	// Docker CLI <= 27: "docker: 'compose' is not a docker command."
	// Docker CLI >= 28: "docker: unknown command: docker compose"
	return strings.Contains(stderr, "'compose' is not a docker command") ||
		strings.Contains(stderr, "unknown command: docker compose")
}
