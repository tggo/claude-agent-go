package transport

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func fakeClaudeCLI(t *testing.T, body string) Local {
	return Local{Binary: writeScript(t, "claude", body)}
}

func TestPreflightOK(t *testing.T) {
	l := fakeClaudeCLI(t, `[ "$1" = --version ] && echo "2.1.318 (Claude Code)"`)
	info, err := Preflight(ctx, l)
	if err != nil || info.Version != "2.1.318" || info.Raw != "2.1.318 (Claude Code)" || info.AuthChecked {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestPreflightTooOld(t *testing.T) {
	l := fakeClaudeCLI(t, `echo "2.1.6 (Claude Code)"`)
	_, err := Preflight(ctx, l)
	if !errors.Is(err, ErrCLITooOld) || !strings.Contains(err.Error(), "2.1.196") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Preflight(ctx, l, PreflightOptions{MinVersion: "none"}); err != nil {
		t.Errorf("none should disable gate: %v", err)
	}
	if _, err := Preflight(ctx, l, PreflightOptions{MinVersion: "2.1.5"}); err != nil {
		t.Errorf("override: %v", err)
	}
	if _, err := Preflight(ctx, l, PreflightOptions{MinVersion: "junk"}); err == nil {
		t.Error("bad min version should error")
	}
}

func TestPreflightNotFound(t *testing.T) {
	for _, tr := range []Transport{
		Local{Binary: "/nonexistent/claude"},
		Local{Binary: "definitely-not-a-binary-xyz"},
		fakeClaudeCLI(t, `echo "sh: claude: not found" >&2; exit 127`),
		fakeClaudeCLI(t, `exit 126`),
	} {
		if _, err := Preflight(ctx, tr); !errors.Is(err, ErrClaudeNotFound) {
			t.Errorf("%T: err = %v", tr, err)
		}
	}
}

func TestPreflightOtherFailures(t *testing.T) {
	_, err := Preflight(ctx, fakeClaudeCLI(t, `echo oops >&2; exit 3`))
	if err == nil || errors.Is(err, ErrClaudeNotFound) || !strings.Contains(err.Error(), "oops") {
		t.Errorf("err = %v", err)
	}
	if _, err := Preflight(ctx, fakeClaudeCLI(t, `echo hello`)); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Errorf("unparseable: %v", err)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Preflight(c, fakeClaudeCLI(t, `sleep 5`)); err == nil {
		t.Error("cancelled ctx should fail")
	}
	start := time.Now()
	_, err = Preflight(ctx, fakeClaudeCLI(t, `sleep 30`), PreflightOptions{Timeout: 200 * time.Millisecond})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("timeout not enforced: %v", err)
	}
}

func authCLI(t *testing.T, status string) Local {
	return fakeClaudeCLI(t, `case "$1" in
--version) echo "2.1.318 (Claude Code)";;
auth) `+status+`;;
esac`)
}

func TestPreflightAuth(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	opt := PreflightOptions{CheckAuth: true}

	info, err := Preflight(ctx, authCLI(t, `echo '{"loggedIn":true,"authMethod":"claude.ai"}'`), opt)
	if err != nil || !info.AuthChecked || info.AuthMethod != "claude.ai" {
		t.Fatalf("%+v %v", info, err)
	}
	_, err = Preflight(ctx, authCLI(t, `echo '{"loggedIn":false}'; exit 1`), opt)
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("logged out: %v", err)
	}
	info, err = Preflight(ctx, authCLI(t, `echo "unknown command" >&2; exit 1`), opt)
	if err != nil || info.AuthChecked {
		t.Errorf("old CLI should be unknown, not failure: %+v %v", info, err)
	}

	// Env credentials short-circuit (no `auth status` call: it would fail here).
	nope := authCLI(t, `exit 1`)
	info, err = Preflight(ctx, nope, PreflightOptions{CheckAuth: true, Env: []string{"ANTHROPIC_API_KEY=sk-x"}})
	if err != nil || info.AuthMethod != "env" || !info.AuthChecked {
		t.Errorf("env: %+v %v", info, err)
	}
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok")
	if info, _ = Preflight(ctx, nope, opt); info.AuthMethod != "env" {
		t.Errorf("host env on Local: %+v", info)
	}
	// Host env is irrelevant to remote targets.
	bin, _ := fakeLauncher(t, "ssh", `echo "2.1.318 (Claude Code)"`)
	info, _ = Preflight(ctx, SSH{Host: "h", SSHBin: bin}, opt)
	if info.AuthMethod == "env" {
		t.Errorf("host env leaked into remote check: %+v", info)
	}
	// Empty env value does not count.
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	info, _ = Preflight(ctx, authCLI(t, `echo '{"loggedIn":true,"authMethod":"x"}'`), PreflightOptions{CheckAuth: true, Env: []string{"ANTHROPIC_API_KEY="}})
	if info.AuthMethod != "x" {
		t.Errorf("empty env counted: %+v", info)
	}
	// auth check cancelled mid-flight surfaces as an error.
	slow := fakeClaudeCLI(t, `case "$1" in --version) echo "2.1.318";; auth) sleep 30;; esac`)
	_, err = Preflight(ctx, slow, PreflightOptions{CheckAuth: true, Timeout: 400 * time.Millisecond})
	if err == nil {
		t.Error("hung auth status should error")
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"2.1.196", "2.1.196", 0}, {"2.1.318", "2.1.196", 1}, {"2.1.9", "2.1.196", -1},
		{"2.2.0", "2.1.999", 1}, {"1.99.99", "2.0.0", -1}, {"v3.0.0 (x)", "2.9.9", 1},
	} {
		got, err := CompareVersions(c.a, c.b)
		if err != nil || got != c.want {
			t.Errorf("%s vs %s = %d %v", c.a, c.b, got, err)
		}
	}
	if _, err := CompareVersions("x", "1.0.0"); err == nil {
		t.Error("want error")
	}
}
