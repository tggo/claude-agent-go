//go:build integration

package client

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntegrationPreToolUseHookFires proves hook declaration via initialize and
// hook_callback dispatch work against the real binary: a PreToolUse hook is
// invoked when the agent uses a tool.
func TestIntegrationPreToolUseHookFires(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	dir := t.TempDir()

	var fired atomic.Int32
	var lastInput atomic.Value

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:    "haiku",
		WorkDir:  dir,
		MaxTurns: 5,
		Hooks: map[string][]HookMatcher{
			"PreToolUse": {{
				Matcher: "", // all tools
				Callbacks: []HookCallback{
					func(_ context.Context, input json.RawMessage, _ string) (json.RawMessage, error) {
						fired.Add(1)
						lastInput.Store(string(input))
						return nil, nil // no-op: allow to proceed
					},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if _, err := c.Query(ctx, "Run the bash command: echo hello-from-hook-test", nil); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if fired.Load() == 0 {
		t.Fatal("PreToolUse hook never fired")
	}
	t.Logf("hook fired %d time(s); last input=%v", fired.Load(), lastInput.Load())
}

// TestIntegrationPreToolUseHookBlocks proves a hook DECISION is honored, not
// just that the hook fires: a PreToolUse hook that denies Bash must prevent the
// command from running. Behavioral signal: the file the command would create
// does not exist.
func TestIntegrationPreToolUseHookBlocks(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "should_not_exist.txt")

	var fired atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:    "haiku",
		WorkDir:  dir,
		MaxTurns: 4,
		Hooks: map[string][]HookMatcher{
			"PreToolUse": {{
				Matcher: "Bash",
				Callbacks: []HookCallback{
					func(_ context.Context, _ json.RawMessage, _ string) (json.RawMessage, error) {
						fired.Add(1)
						return json.RawMessage(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"blocked by integration test"}}`), nil
					},
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	_, err = c.Query(ctx, "Run exactly this bash command and nothing else: touch "+sentinel, nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if fired.Load() == 0 {
		t.Fatal("blocking hook never fired")
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Errorf("hook denied Bash but the command still ran (file created): %s", sentinel)
	}
	t.Logf("hook fired %d time(s) and blocked the command; file absent=%v", fired.Load(), os.IsNotExist(func() error { _, e := os.Stat(sentinel); return e }()))
}
