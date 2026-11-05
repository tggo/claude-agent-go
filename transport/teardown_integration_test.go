//go:build integration

package transport

import (
	"context"
	"fmt"
	"math/rand"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// stubborn is a stand-in for claude: ignores SIGTERM, owns a child with a
// unique, greppable argv, and never exits on its own. It is passed as args to
// `sh` (Binary "sh") so nothing needs to exist on the target besides a shell.
func stubborn(marker int) []string {
	return []string{"-c", fmt.Sprintf("trap '' TERM HUP; sleep %d & while :; do sleep 1; done", marker)}
}

func newMarker() int { return 40000 + rand.Intn(20000) }

func eventually(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}

// runThenAbandon launches cmd, waits until the remote shows the marker, then
// SIGKILLs the LOCAL proxy only — the situation that used to orphan claude.
func runThenAbandon(t *testing.T, cmd *exec.Cmd, visible func() bool) {
	t.Helper()
	runThenAbandonOpt(t, cmd, visible, true)
}

// runThenAbandonOpt is runThenAbandon; with abandon=false the local proxy is
// left connected (Terminate is then what must end the whole chain). The proxy
// is always reaped on test cleanup.
func runThenAbandonOpt(t *testing.T, cmd *exec.Cmd, visible func() bool, abandon bool) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if !eventually(t, 20*time.Second, visible) {
		_ = cmd.Process.Kill()
		t.Fatal("remote process never appeared")
	}
	if !abandon {
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return
	}
	_ = cmd.Process.Kill()
	<-done
	if !visible() {
		t.Log("note: remote died with the local proxy already (no orphan to clean)")
	}
}

func TestIntegrationDockerExecTerminate(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker daemon not available")
	}
	_ = exec.Command("docker", "pull", "-q", "busybox:latest").Run()
	const ctr = "cag-it-dockerexec"
	_ = exec.Command("docker", "rm", "-f", ctr).Run()
	if out, err := exec.Command("docker", "run", "-d", "--init", "--name", ctr, "busybox:latest", "sleep", "300").CombinedOutput(); err != nil {
		t.Skipf("cannot start busybox container: %v %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", ctr).Run()

	marker := newMarker()
	needle := fmt.Sprintf("sleep %d", marker)
	visible := func() bool {
		out, _ := exec.Command("docker", "exec", ctr, "ps").CombinedOutput()
		return strings.Contains(string(out), needle)
	}

	d := DockerExec{Container: ctr, Binary: "sh"}
	id := NewRunID()
	runThenAbandon(t, d.Command(context.Background(), stubborn(marker), CommandOpts{RunID: id}), visible)

	c, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	if err := d.Terminate(c, id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	t.Logf("Terminate took %v", time.Since(start))
	if !eventually(t, 5*time.Second, func() bool { return !visible() }) {
		out, _ := exec.Command("docker", "exec", ctr, "ps").CombinedOutput()
		t.Fatalf("process tagged %s survived Terminate:\n%s", id, out)
	}
	// Idempotent.
	if err := d.Terminate(c, id); err != nil {
		t.Errorf("second Terminate: %v", err)
	}
}

func TestIntegrationDockerRunTerminate(t *testing.T) {
	if !dockerAvailable() {
		t.Skip("docker daemon not available")
	}
	_ = exec.Command("docker", "pull", "-q", "busybox:latest").Run()
	id := NewRunID()
	name := "cag-" + id
	defer exec.Command("docker", "rm", "-f", name).Run()

	exists := func() bool {
		out, _ := exec.Command("docker", "ps", "-a", "-q", "--filter", "name="+name).Output()
		return strings.TrimSpace(string(out)) != ""
	}
	d := DockerRun{Image: "busybox:latest", Binary: "sh"}
	runThenAbandon(t, d.Command(context.Background(), stubborn(newMarker()), CommandOpts{RunID: id}), exists)

	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := d.Terminate(c, id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !eventually(t, 10*time.Second, func() bool { return !exists() }) {
		t.Fatalf("container %s still present after Terminate", name)
	}
	// Already gone is not an error.
	if err := d.Terminate(c, id); err != nil {
		t.Errorf("Terminate of gone container: %v", err)
	}
}

func TestIntegrationSSHTerminate(t *testing.T) {
	opts := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5"}
	if err := exec.Command("ssh", append(append([]string{}, opts...), "localhost", "true")...).Run(); err != nil {
		t.Skip("`ssh -o BatchMode=yes localhost true` fails: add your public key to ~/.ssh/authorized_keys and enable Remote Login to run this test")
	}
	marker := newMarker()
	needle := fmt.Sprintf("sleep %d", marker)
	visible := func() bool {
		out, _ := exec.Command("ps", "-axo", "command").Output()
		return strings.Contains(string(out), needle)
	}
	s := SSH{Host: "localhost", Binary: "sh", Options: opts}
	id := NewRunID()
	// On localhost, sshd tears the remote down together with a SIGKILLed client
	// (no PTY), so there is no orphan to demonstrate; keep the connection open
	// and assert Terminate ends the remote process itself.
	runThenAbandonOpt(t, s.Command(context.Background(), stubborn(marker), CommandOpts{RunID: id}), visible, false)

	c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Terminate(c, id); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !eventually(t, 5*time.Second, func() bool { return !visible() }) {
		_ = exec.Command("pkill", "-f", needle).Run()
		t.Fatalf("remote process survived Terminate")
	}
}
