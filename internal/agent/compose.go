package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// runCompose runs `docker compose -f <file> <args...>` in the given working directory.
// Falls back to `docker-compose` if the compose plugin is unavailable.
func runCompose(ctx context.Context, workDir, composeFile string, args ...string) error {
	baseArgs := []string{"compose"}
	if composeFile != "" {
		baseArgs = append(baseArgs, "-f", composeFile)
	}
	baseArgs = append(baseArgs, args...)

	cmd := exec.CommandContext(ctx, "docker", baseArgs...)
	cmd.Dir = workDir

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// try legacy docker-compose binary
		legacyArgs := make([]string, 0, len(args)+2)
		if composeFile != "" {
			legacyArgs = append(legacyArgs, "-f", composeFile)
		}
		legacyArgs = append(legacyArgs, args...)
		cmd2 := exec.CommandContext(ctx, "docker-compose", legacyArgs...)
		cmd2.Dir = workDir
		var stderr2 bytes.Buffer
		cmd2.Stderr = &stderr2
		if err2 := cmd2.Run(); err2 != nil {
			return fmt.Errorf("%w: %s", err, stderr.String())
		}
	}
	return nil
}
