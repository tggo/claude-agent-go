//go:build integration

package runner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/transport"
)

// TestIntegrationSSHTransport drives the real binary through the SSH transport,
// pointed at localhost. It proves the transport seam works end to end: the
// command is assembled, ssh forwards stdin/stdout, and claude actually runs and
// emits structured output. The final SSHPONG assertion is skipped when the
// remote (non-login) ssh session isn't authenticated — a deployment concern,
// not a transport one.
func TestIntegrationSSHTransport(t *testing.T) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	sshOpts := []string{"-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no"}
	if err := exec.Command("ssh", append(append([]string{}, sshOpts...), "localhost", "true")...).Run(); err != nil {
		t.Skip("ssh localhost not available non-interactively")
	}

	tr := transport.SSH{Host: "localhost", Binary: bin, Options: sshOpts}

	// Probe: run claude through the transport and confirm we get its structured
	// output back over ssh. This alone proves the transport reaches the binary.
	probe := tr.Command(context.Background(),
		[]string{"--output-format", "json", "--verbose", "--dangerously-skip-permissions", "--model", "haiku", "--max-turns", "1"},
		transport.CommandOpts{})
	probe.Stdin = strings.NewReader("Reply with exactly: SSHPONG")
	out, _ := probe.CombinedOutput()
	s := string(out)

	if !strings.Contains(s, `"session_id"`) {
		t.Fatalf("ssh transport did not reach claude; output: %s", truncate(s, 300))
	}
	t.Log("ssh transport reached claude and got structured stream-json back")

	if strings.Contains(s, "authentication_failed") || strings.Contains(s, "Not logged in") {
		t.Skip("transport verified; remote claude unauthenticated over non-login ssh (expected) — auth is a deployment concern")
	}

	// Authenticated remote: assert the full RunJSON path through the transport.
	r := New(WithModel("haiku"), WithTimeout(2*time.Minute), WithTransport(tr))
	res, err := r.RunJSON(context.Background(), Input{Prompt: "Reply with exactly this word and nothing else: SSHPONG"})
	if err != nil {
		t.Fatalf("RunJSON over ssh: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res.ResultText), "SSHPONG") {
		t.Errorf("result = %q", res.ResultText)
	}
	t.Logf("ssh transport end-to-end ok: result=%q cost=$%.5f", res.ResultText, res.TotalCostUSD)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
