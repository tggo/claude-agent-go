package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// fakeClient writes an executable that speaks the bidirectional stream-json
// protocol: for each user line on stdin it emits init/assistant/result;
// control_request lines are ignored. Returns the binary path.
func fakeClient(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *control_request*)
      rid=$(printf '%s' "$line" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')
      printf '%s\n' "{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"$rid\"}}"
      continue ;;
  esac
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"fake-sess"}'
  printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"ack"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"fake-sess","result":"REPLY","total_cost_usd":0.01,"num_turns":1}'
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return path
}

func TestClientLifecycle(t *testing.T) {
	bin := fakeClient(t)
	ctx := context.Background()
	c, err := New(ctx, Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var sawResult bool
	turn, err := c.Query(ctx, "hello", func(ev claudecli.StreamEvent) {
		if ev.IsResult() {
			sawResult = true
		}
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if turn.Text != "REPLY" || turn.SessionID != "fake-sess" || turn.TotalCostUSD != 0.01 || turn.NumTurns != 1 {
		t.Errorf("turn = %+v", turn)
	}
	if !sawResult {
		t.Error("onEvent never saw result")
	}
	if len(turn.Events) < 3 {
		t.Errorf("events = %d, want >=3", len(turn.Events))
	}

	// second turn reuses the same process
	if _, err := c.Query(ctx, "again", nil); err != nil {
		t.Fatalf("second Query: %v", err)
	}

	// interrupt is best-effort; should not error against a live process
	if err := c.Interrupt(ctx); err != nil {
		t.Errorf("Interrupt: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// idempotent
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// query after close
	if _, err := c.Query(ctx, "x", nil); err == nil {
		t.Error("Query after Close should error")
	}
}

// fakeClientEmptyResult emits a result with no "result" text, forcing Query to
// fall back to concatenated assistant text.
func fakeClientEmptyResult(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"fallback text"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"s","num_turns":1}'
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return path
}

func TestClientResultTextFallback(t *testing.T) {
	bin := fakeClientEmptyResult(t)
	ctx := context.Background()
	c, err := New(ctx, Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	turn, err := c.Query(ctx, "hi", nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if turn.Text != "fallback text" {
		t.Errorf("Text = %q, want fallback text", turn.Text)
	}
}

func TestClientQueryContextCancel(t *testing.T) {
	bin := fakeClient(t)
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	if _, err := c.Query(ctx, "hi", nil); err == nil {
		t.Error("Query with cancelled ctx should error")
	}
}

func TestClientWait(t *testing.T) {
	bin := fakeClient(t)
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Wait did not return after Close")
	}
}

func TestApplyClientDefaults(t *testing.T) {
	cfg := Config{}
	applyClientDefaults(&cfg)
	if cfg.Binary != defaultBinary || cfg.Model != defaultModel ||
		cfg.MaxTurns != defaultTurns || cfg.StartTimeout != defaultStartTO || cfg.Logger == nil {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if !cfg.SkipPermissions {
		t.Error("SkipPermissions should default true when no PermissionMode set")
	}

	// With an explicit permission mode and SkipPermissions left false, it stays false.
	cfg2 := Config{PermissionMode: "plan"}
	applyClientDefaults(&cfg2)
	if cfg2.SkipPermissions {
		t.Error("SkipPermissions should stay false when PermissionMode is set")
	}
}

func TestBuildClientArgs(t *testing.T) {
	cfg := Config{
		Model:           "opus",
		MaxTurns:        12,
		SkipPermissions: true,
		PermissionMode:  "acceptEdits",
		MCPConfigPath:   "/mcp.json",
		AllowedTools:    []string{"Read", "Bash"},
		SystemPrompt:    "sys",
		ExtraArgs:       []string{"--foo"},
	}
	line := " " + strings.Join(buildClientArgs(cfg), " ") + " "
	for _, want := range []string{
		"--input-format stream-json",
		"--output-format stream-json",
		"--verbose",
		"--model opus",
		"--max-turns 12",
		"--dangerously-skip-permissions",
		"--permission-mode acceptEdits",
		"--mcp-config /mcp.json",
		"--allowedTools Read",
		"--allowedTools Bash",
		"--system-prompt sys",
		"--foo",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
}

func TestBuildClientArgsStreamingFlags(t *testing.T) {
	line := " " + strings.Join(buildClientArgs(Config{
		Model:                  "haiku",
		MaxTurns:               5,
		IncludePartialMessages: true,
		IncludeHookEvents:      true,
	}), " ") + " "
	for _, want := range []string{"--include-partial-messages", "--include-hook-events"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
}

// fakeClientExits reads one line then exits without emitting a result, so Query
// observes the process ending before a result.
func fakeClientExits(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nread line\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestClientQueryProcessEnds(t *testing.T) {
	bin := fakeClientExits(t)
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if _, err := c.Query(context.Background(), "hi", nil); err == nil {
		t.Error("Query should error when process ends before result")
	}
}

func TestClosedClientRejects(t *testing.T) {
	// A zero Client with closed set behaves as closed without a live process.
	c := &Client{}
	c.closed.Store(true)
	if _, err := c.Query(context.Background(), "hi", nil); err == nil {
		t.Error("Query on closed client should error")
	}
	if err := c.Interrupt(context.Background()); err == nil {
		t.Error("Interrupt on closed client should error")
	}
}
