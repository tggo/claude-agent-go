package client

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func noopHook(context.Context, json.RawMessage, string) (json.RawMessage, error) {
	return nil, nil
}

func TestHookBuilder(t *testing.T) {
	h := NewHooks().
		PreToolUse("Bash", noopHook).
		PreToolUse("Bash", noopHook). // same event+matcher -> appended
		PreToolUse("Write", noopHook).
		PostToolUse("", noopHook).
		On("Stop", "", noopHook).
		Build()

	pre := h["PreToolUse"]
	if len(pre) != 2 {
		t.Fatalf("PreToolUse matchers = %d, want 2 (Bash, Write)", len(pre))
	}
	var bash *HookMatcher
	for i := range pre {
		if pre[i].Matcher == "Bash" {
			bash = &pre[i]
		}
	}
	if bash == nil || len(bash.Callbacks) != 2 {
		t.Errorf("Bash callbacks = %v", bash)
	}
	if len(h["PostToolUse"]) != 1 || len(h["Stop"]) != 1 {
		t.Errorf("PostToolUse/Stop wrong: %v", h)
	}
}

func TestRunCallbackTimeout(t *testing.T) {
	c := &Client{cfg: Config{CallbackTimeout: 100 * time.Millisecond}}
	// A callback that ignores ctx and blocks past the timeout.
	_, err := c.runCallback(func(ctx context.Context) (map[string]any, error) {
		time.Sleep(500 * time.Millisecond)
		return map[string]any{"behavior": "allow"}, nil
	})
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunCallbackFast(t *testing.T) {
	c := &Client{cfg: Config{CallbackTimeout: time.Second}}
	data, err := c.runCallback(func(ctx context.Context) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil || data["ok"] != true {
		t.Errorf("fast callback: data=%v err=%v", data, err)
	}
}
