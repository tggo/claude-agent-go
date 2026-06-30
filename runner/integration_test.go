//go:build integration

// Integration tests that invoke the real `claude` binary. Run with:
//
//	go test -tags integration ./runner/...
//
// Requires `claude` on PATH and valid credentials. Uses the cheap haiku model.
package runner

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
}

func haikuRunner() *Runner {
	return New(
		WithModel("haiku"),
		WithMaxTurns(3),
		WithTimeout(2*time.Minute),
		WithEntrypoint("claude-agent-go-itest"),
	)
}

func TestIntegrationRun(t *testing.T) {
	requireClaude(t)
	r := haikuRunner()
	res, err := r.Run(context.Background(), Input{
		Prompt: "Reply with exactly this word and nothing else: PONG",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res.Response), "PONG") {
		t.Errorf("response = %q, want it to contain PONG", res.Response)
	}
}

func TestIntegrationRunJSON(t *testing.T) {
	requireClaude(t)
	r := haikuRunner()
	res, err := r.RunJSON(context.Background(), Input{
		Prompt: "Reply with exactly this word and nothing else: ALPHA",
	})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res.ResultText), "ALPHA") {
		t.Errorf("result = %q", res.ResultText)
	}
	if res.SessionID == "" {
		t.Error("expected a session id")
	}
	if res.TotalCostUSD <= 0 {
		t.Errorf("expected positive cost, got %v", res.TotalCostUSD)
	}
	if res.Metadata == nil || res.Metadata.Model == "" {
		t.Error("expected metadata with model")
	}
	t.Logf("session=%s cost=$%.5f turns=%d model=%s", res.SessionID, res.TotalCostUSD, res.NumTurns, res.Metadata.Model)
}

func TestIntegrationRunStream(t *testing.T) {
	requireClaude(t)
	r := haikuRunner()

	var events int
	var sawResult bool
	res, err := r.RunStream(context.Background(), Input{
		Prompt: "Reply with exactly this word and nothing else: BETA",
	}, func(ev claudecli.StreamEvent, n int) {
		events = n
		if ev.IsResult() {
			sawResult = true
		}
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if !strings.Contains(strings.ToUpper(res.ResultText), "BETA") {
		t.Errorf("result = %q", res.ResultText)
	}
	if !sawResult {
		t.Error("never saw a result event")
	}
	if events == 0 {
		t.Error("no stream events observed")
	}
	if res.TotalCostUSD <= 0 {
		t.Errorf("expected positive cost, got %v", res.TotalCostUSD)
	}
	t.Logf("events=%d cost=$%.5f", events, res.TotalCostUSD)
}

// TestIntegrationResume proves --resume actually continues a prior session
// across two separate processes: turn 2 (a fresh process) recalls a fact
// established in turn 1's session, which is only possible if resume works.
func TestIntegrationResume(t *testing.T) {
	requireClaude(t)
	r := haikuRunner()
	ctx := context.Background()

	first, err := r.RunJSON(ctx, Input{
		Prompt: "Remember this codeword: PURPLE-FALCON-88. Reply with exactly: OK",
	})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.SessionID == "" {
		t.Fatal("no session id from first run")
	}

	// Second, separate process — resume the same session and ask it to recall.
	second, err := r.RunJSON(ctx, Input{
		Prompt: "What codeword did I ask you to remember? Reply with only the codeword.",
		Resume: first.SessionID,
	})
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if !strings.Contains(second.ResultText, "PURPLE-FALCON-88") {
		t.Errorf("resumed session did not recall the codeword (resume had no effect): %q", second.ResultText)
	}
	t.Logf("resume worked: session=%s recalled=%q", first.SessionID, strings.TrimSpace(second.ResultText))
}
