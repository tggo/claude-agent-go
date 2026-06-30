package runner

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// fakeClaude writes an executable shell script at <dir>/claude that emits body
// to stdout, and returns its path. Skips on Windows (no /bin/sh).
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\ncat <<'EOF'\n" + body + "\nEOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestRunPlain(t *testing.T) {
	bin := fakeClaude(t, "hello from claude")
	r := New(WithBinary(bin))

	res, err := r.Run(context.Background(), Input{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Response != "hello from claude" {
		t.Fatalf("Response = %q", res.Response)
	}
}

func TestRunJSON(t *testing.T) {
	out := `[{"type":"system","subtype":"init","session_id":"sess-1","model":"sonnet"},` +
		`{"type":"result","session_id":"sess-1","result":"done","total_cost_usd":0.0123,"num_turns":3}]`
	bin := fakeClaude(t, out)
	r := New(WithBinary(bin))

	res, err := r.RunJSON(context.Background(), Input{Prompt: "hi"})
	if err != nil {
		t.Fatalf("RunJSON: %v", err)
	}
	if res.ResultText != "done" {
		t.Errorf("ResultText = %q", res.ResultText)
	}
	if res.SessionID != "sess-1" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	if res.TotalCostUSD != 0.0123 {
		t.Errorf("TotalCostUSD = %v", res.TotalCostUSD)
	}
	if res.NumTurns != 3 {
		t.Errorf("NumTurns = %d", res.NumTurns)
	}
	if res.Metadata == nil {
		t.Error("Metadata is nil")
	}
}

func TestRunStream(t *testing.T) {
	out := `{"type":"system","subtype":"init","session_id":"sess-2"}
{"type":"assistant","subtype":"text","message":"thinking..."}
{"type":"result","session_id":"sess-2","result":"final answer","cost_usd":0.05,"num_turns":2}`
	bin := fakeClaude(t, out)
	r := New(WithBinary(bin))

	var events int
	var sawResult bool
	res, err := r.RunStream(context.Background(), Input{Prompt: "hi"},
		func(ev claudecli.StreamEvent, n int) {
			events = n
			if ev.IsResult() {
				sawResult = true
			}
		})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	if !sawResult {
		t.Error("progress never saw a result event")
	}
	if events != 3 {
		t.Errorf("events = %d, want 3", events)
	}
	if res.ResultText != "final answer" {
		t.Errorf("ResultText = %q", res.ResultText)
	}
	if res.SessionID != "sess-2" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	if res.TotalCostUSD != 0.05 {
		t.Errorf("TotalCostUSD = %v", res.TotalCostUSD)
	}
}

func TestRunEmptyPrompt(t *testing.T) {
	r := New(WithBinary("/nonexistent"))
	if _, err := r.Run(context.Background(), Input{Prompt: "   "}); err == nil {
		t.Fatal("expected error for empty prompt")
	}
	if _, err := r.RunJSON(context.Background(), Input{Prompt: ""}); err == nil {
		t.Fatal("RunJSON expected error for empty prompt")
	}
	if _, err := r.RunStream(context.Background(), Input{Prompt: ""}, nil); err == nil {
		t.Fatal("RunStream expected error for empty prompt")
	}
}

// failingClaude exits non-zero after writing to stderr.
func failingClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'boom' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestRunFailures(t *testing.T) {
	bin := failingClaude(t)
	r := New(WithBinary(bin))
	ctx := context.Background()
	if _, err := r.Run(ctx, Input{Prompt: "x"}); err == nil {
		t.Error("Run should error on non-zero exit")
	}
	if _, err := r.RunJSON(ctx, Input{Prompt: "x"}); err == nil {
		t.Error("RunJSON should error on non-zero exit")
	}
	if _, err := r.RunStream(ctx, Input{Prompt: "x"}, nil); err == nil {
		t.Error("RunStream should error on non-zero exit")
	}
}

// slowClaude sleeps far longer than the test timeout.
func slowClaude(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestRunTimeouts(t *testing.T) {
	bin := slowClaude(t)
	r := New(WithBinary(bin), WithTimeout(300*time.Millisecond))
	ctx := context.Background()
	if _, err := r.Run(ctx, Input{Prompt: "x"}); err == nil {
		t.Error("Run should time out")
	}
	if _, err := r.RunStream(ctx, Input{Prompt: "x"}, nil); err == nil {
		t.Error("RunStream should time out")
	}
}

func TestRunBadJSON(t *testing.T) {
	bin := fakeClaude(t, "this is not json at all")
	r := New(WithBinary(bin))
	if _, err := r.RunJSON(context.Background(), Input{Prompt: "x"}); err == nil {
		t.Error("RunJSON should error on unparseable output")
	}
}

func TestBuildArgs(t *testing.T) {
	r := New(
		WithModel("opus"),
		WithMaxTurns(10),
		WithMaxBudgetUSD("2.50"),
		WithAllowedTools("Read", "Bash"),
	)
	args := r.buildArgs(Input{
		SystemPrompt:  "be terse",
		ContextFiles:  []string{"a.md"},
		MCPConfigPath: "/tmp/mcp.json",
		Model:         "haiku",
	}, modeStream)

	joined := " " + join(args, " ") + " "
	wantContains := []string{
		"--output-format stream-json",
		"--dangerously-skip-permissions",
		"--model haiku", // input overrides config
		"--max-turns 10",
		"--max-budget-usd 2.50",
		"--mcp-config /tmp/mcp.json",
		"--allowedTools Read",
		"--allowedTools Bash",
		"--add-context a.md",
		"--system-prompt be terse",
	}
	for _, w := range wantContains {
		if !contains(joined, w) {
			t.Errorf("args missing %q; got: %s", w, joined)
		}
	}
}

// tiny local helpers to avoid extra imports in the test.
func join(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
outer:
	for i := 0; i+len(sub) <= len(s); i++ {
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
