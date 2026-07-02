//go:build integration

package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIntegrationDockerReachesToolsServer proves a container can reach the
// in-process tools server on the host via the host.docker.internal mapping —
// the reachability the reverse-tunnel helpers set up. It starts a real tools
// server and, from inside a busybox container, fetches its URL; getting any HTTP
// response back (not "connection refused") proves the container reached it.
func TestIntegrationDockerReachesToolsServer(t *testing.T) {
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker daemon not available")
	}
	_ = exec.Command("docker", "pull", "busybox:latest").Run()

	srv, err := Serve("reach", Tool{Name: "noop", Description: "noop",
		Handler: func(context.Context, json.RawMessage) (string, error) { return "ok", nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	url := "http://host.docker.internal:" + strconv.Itoa(srv.Port()) + "/"
	args := append([]string{"run", "-i", "--rm"}, DockerHostGateway()...)
	args = append(args, "busybox:latest", "sh", "-c", "wget -S -T 5 -O /dev/null "+url+" 2>&1 || true")
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker run: %v (%s)", err, out)
	}
	got := string(out)
	if strings.Contains(strings.ToLower(got), "refused") || strings.Contains(strings.ToLower(got), "bad address") {
		t.Fatalf("container could not reach the host tools server:\n%s", got)
	}
	if !strings.Contains(got, "HTTP/") {
		t.Fatalf("no HTTP response from the tools server (not reached?):\n%s", got)
	}
	t.Logf("container reached host tools server via host.docker.internal (got an HTTP response)")
}
