package runner

import (
	"strings"
	"testing"
	"time"
)

func argLine(args []string) string { return " " + strings.Join(args, " ") + " " }

func TestBuildArgsModes(t *testing.T) {
	r := New()
	if !strings.Contains(argLine(r.buildArgs(Input{}, modePlain)), " --print ") {
		t.Error("plain mode missing --print")
	}
	if !strings.Contains(argLine(r.buildArgs(Input{}, modeJSON)), " --output-format json ") {
		t.Error("json mode missing --output-format json")
	}
	if !strings.Contains(argLine(r.buildArgs(Input{}, modeStream)), " --output-format stream-json ") {
		t.Error("stream mode missing --output-format stream-json")
	}
}

func TestBuildArgsBudgetExcludedInPlain(t *testing.T) {
	r := New(WithMaxBudgetUSD("3.00"))
	if strings.Contains(argLine(r.buildArgs(Input{}, modePlain)), "--max-budget-usd") {
		t.Error("plain mode should not include --max-budget-usd")
	}
	if !strings.Contains(argLine(r.buildArgs(Input{}, modeJSON)), "--max-budget-usd 3.00") {
		t.Error("json mode should include --max-budget-usd")
	}
}

func TestBuildArgsAllFlags(t *testing.T) {
	r := New(WithAllowedTools("Read"))
	args := r.buildArgs(Input{
		PermissionMode: "acceptEdits",
		Resume:         "sess-9",
		Continue:       true,
		SettingsPath:   "/s.json",
		MCPConfigPath:  "/mcp.json",
		AddDirs:        []string{"/extra"},
		ContextFiles:   []string{"c.md"},
		SystemPrompt:   "sys",
		ExtraArgs:      []string{"--foo", "bar"},
	}, modeStream)
	line := argLine(args)
	for _, want := range []string{
		"--permission-mode acceptEdits",
		"--resume sess-9",
		"--continue",
		"--settings /s.json",
		"--mcp-config /mcp.json",
		"--allowedTools Read",
		"--add-dir /extra",
		"--add-context c.md",
		"--system-prompt sys",
		"--foo bar",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in: %s", want, line)
		}
	}
	// ExtraArgs must come last.
	if !strings.HasSuffix(strings.TrimSpace(line), "--foo bar") {
		t.Errorf("ExtraArgs not last: %s", line)
	}
}

func TestBuildArgsPartialAndHookEvents(t *testing.T) {
	r := New(WithPartialMessages(true), WithHookEvents(true))
	stream := argLine(r.buildArgs(Input{}, modeStream))
	if !strings.Contains(stream, "--include-partial-messages") {
		t.Error("stream mode missing --include-partial-messages")
	}
	if !strings.Contains(stream, "--include-hook-events") {
		t.Error("stream mode missing --include-hook-events")
	}
	// These flags must NOT appear in json/plain modes.
	js := argLine(r.buildArgs(Input{}, modeJSON))
	if strings.Contains(js, "--include-partial-messages") || strings.Contains(js, "--include-hook-events") {
		t.Error("json mode should not include partial/hook flags")
	}
}

func TestBuildArgsSkipPermissionsOff(t *testing.T) {
	r := New(WithSkipPermissions(false))
	if strings.Contains(argLine(r.buildArgs(Input{}, modeJSON)), "--dangerously-skip-permissions") {
		t.Error("skip-permissions should be absent when disabled")
	}
}

func TestDefaultsApplied(t *testing.T) {
	r := New()
	if r.cfg.Binary != defaultBinary || r.cfg.DefaultModel != defaultModel ||
		r.cfg.MaxTurns != defaultMaxTurns || r.cfg.MaxBudgetUSD != defaultBudgetUSD ||
		r.cfg.ProcessTimeout != defaultTimeout || r.cfg.Logger == nil {
		t.Errorf("defaults not applied: %+v", r.cfg)
	}
	if !r.cfg.skipPermissions {
		t.Error("skipPermissions should default to true")
	}
}

func TestOptionsAndNewWithConfig(t *testing.T) {
	r := NewWithConfig(Config{DefaultModel: "opus"},
		WithBinary("/bin/claude"),
		WithTimeout(2*time.Minute),
		WithEntrypoint("ep"),
		WithEnv("A=1", "B=2"),
		WithMaxTurns(7),
	)
	if r.cfg.Binary != "/bin/claude" || r.cfg.DefaultModel != "opus" ||
		r.cfg.ProcessTimeout != 2*time.Minute || r.cfg.Entrypoint != "ep" ||
		len(r.cfg.Env) != 2 || r.cfg.MaxTurns != 7 {
		t.Errorf("options not applied: %+v", r.cfg)
	}
}

func TestModelOf(t *testing.T) {
	r := New(WithModel("opus"))
	if got := r.modelOf(Input{}); got != "opus" {
		t.Errorf("modelOf default = %q", got)
	}
	if got := r.modelOf(Input{Model: "haiku"}); got != "haiku" {
		t.Errorf("modelOf override = %q", got)
	}
}

func TestSanitizeOutput(t *testing.T) {
	got := sanitizeOutput([]byte("clone https://x-access-token:ghp_secret@github.com/o/r.git ok"))
	if strings.Contains(got, "ghp_secret") {
		t.Errorf("token not redacted: %s", got)
	}
	if !strings.Contains(got, "x-access-token:***") {
		t.Errorf("redaction marker missing: %s", got)
	}
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'a'
	}
	if out := sanitizeOutput(long); len(out) > 2100 || !strings.HasSuffix(out, "(truncated)") {
		t.Errorf("long output not truncated: len=%d", len(out))
	}
}
