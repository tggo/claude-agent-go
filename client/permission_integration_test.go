//go:build integration

package client

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestIntegrationCanUseToolDeny proves the can_use_tool control callback fires
// against the real binary and that a deny actually blocks the tool.
func TestIntegrationCanUseToolDeny(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "should_not_exist.txt")

	var calls atomic.Int32
	var sawWrite atomic.Bool

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:    "haiku",
		WorkDir:  dir,
		MaxTurns: 5,
		CanUseTool: func(_ context.Context, tool string, input json.RawMessage, _ PermissionContext) (PermissionResult, error) {
			calls.Add(1)
			if tool == "Write" {
				sawWrite.Store(true)
				return Deny("writing is not allowed in this test"), nil
			}
			return Allow(), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	turn, err := c.Query(ctx, "Use the Write tool to create the file "+target+" with content: hi", nil)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("can_use_tool callback never fired")
	}
	if !sawWrite.Load() {
		t.Error("expected a Write permission request")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Errorf("file was created despite deny: %s", target)
	}
	t.Logf("callback fired %d time(s); denied Write; final=%q", calls.Load(), strings.TrimSpace(turn.Text))
}

// TestIntegrationCanUseToolAllow proves an allow lets the tool run.
func TestIntegrationCanUseToolAllow(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "allowed.txt")

	var calls atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := New(ctx, Config{
		Model:    "haiku",
		WorkDir:  dir,
		MaxTurns: 6,
		CanUseTool: func(_ context.Context, _ string, _ json.RawMessage, _ PermissionContext) (PermissionResult, error) {
			calls.Add(1)
			return Allow(), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	if _, err := c.Query(ctx, "Use the Write tool to create the file "+target+" containing exactly: ok", nil); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if calls.Load() == 0 {
		t.Fatal("can_use_tool callback never fired")
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("file was not created despite allow: %v", statErr)
	}
	t.Logf("callback fired %d time(s); file created", calls.Load())
}
