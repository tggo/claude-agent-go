//go:build integration

package transport

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func dockerAvailable() bool {
	return exec.Command("docker", "info").Run() == nil
}

// TestIntegrationDockerRun proves the DockerRun transport end to end against the
// real docker daemon: it launches an ephemeral container, forwards stdin into
// it, captures the container's stdout, and --rm removes the container after —
// the same mechanics that carry a real `claude` run, verified with a stock image
// (busybox `cat`, which echoes stdin) so no claude-in-image is required.
func TestIntegrationDockerRun(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker daemon not available")
	}
	// Best-effort ensure the image exists; ignore failure (may be cached/offline).
	_ = exec.Command("docker", "pull", "busybox:latest").Run()

	const name = "claude-agent-go-dockerrun-it"
	_ = exec.Command("docker", "rm", "-f", name).Run() // clean any leftover

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tr := DockerRun{Image: "busybox:latest", Binary: "cat", Name: name}
	cmd := tr.Command(ctx, nil, CommandOpts{})
	cmd.Stdin = strings.NewReader("hello-from-dockerrun\n")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker run failed: %v", err)
	}
	if !strings.Contains(string(out), "hello-from-dockerrun") {
		t.Errorf("stdin was not forwarded through the container; got %q", out)
	}
	t.Logf("DockerRun forwarded stdin/stdout through an ephemeral container")

	// --rm teardown: no container with our name should remain.
	ps, _ := exec.Command("docker", "ps", "-a", "-q", "--filter", "name="+name).Output()
	if strings.TrimSpace(string(ps)) != "" {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		t.Errorf("container was not removed by --rm: %q", ps)
	}
}
