package transport

import (
	"context"
	"slices"
	"strings"
	"testing"
)

var ctx = context.Background()

func TestLocal(t *testing.T) {
	c := Local{Binary: "/usr/bin/claude"}.Command(ctx,
		[]string{"--print", "--model", "haiku"},
		CommandOpts{WorkDir: "/repo", Env: []string{"FOO=bar"}})

	if c.Args[0] != "/usr/bin/claude" || c.Args[1] != "--print" {
		t.Errorf("args = %v", c.Args)
	}
	if c.Dir != "/repo" {
		t.Errorf("dir = %q", c.Dir)
	}
	if !slices.Contains(c.Env, "FOO=bar") {
		t.Errorf("env missing extra: %v", c.Env[len(c.Env)-3:])
	}
	// host env is merged in (PATH almost always present)
	if len(c.Env) < 2 {
		t.Errorf("expected host env merged, got %d entries", len(c.Env))
	}
}

func TestLocalDefaultBinary(t *testing.T) {
	c := Local{}.Command(ctx, []string{"--print"}, CommandOpts{})
	if !strings.HasSuffix(c.Args[0], "claude") {
		t.Errorf("default binary = %q", c.Args[0])
	}
}

func TestDockerExec(t *testing.T) {
	c := DockerExec{Container: "agent-box", User: "node"}.Command(ctx,
		[]string{"--print", "--model", "haiku"},
		CommandOpts{WorkDir: "/work", Env: []string{"GH_TOKEN=x", "A=b"}})

	got := strings.Join(c.Args, " ")
	for _, want := range []string{
		"docker exec -i",
		"-w /work",
		"-u node",
		"-e GH_TOKEN=x",
		"-e A=b",
		"agent-box claude --print --model haiku",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	if !strings.HasSuffix(c.Path, "docker") && c.Path != "docker" {
		t.Errorf("path = %q, want docker", c.Path)
	}
}

func TestDockerExecDefaults(t *testing.T) {
	c := DockerExec{Container: "box"}.Command(ctx, []string{"--print"}, CommandOpts{})
	got := strings.Join(c.Args, " ")
	// no -w / -u / -e when not configured
	if strings.Contains(got, "-w") || strings.Contains(got, "-u") || strings.Contains(got, "-e") {
		t.Errorf("unexpected flags: %s", got)
	}
	if !strings.Contains(got, "box claude --print") {
		t.Errorf("got: %s", got)
	}
}

func TestSSH(t *testing.T) {
	c := SSH{Host: "user@host", Port: "2222", Options: []string{"-tt"}}.Command(ctx,
		[]string{"--print", "--system-prompt", "be terse"},
		CommandOpts{WorkDir: "/srv/repo", Env: []string{"K=v with space"}})

	// argv: ssh -p 2222 -tt user@host "<remote>"
	if c.Args[0] != "ssh" || !slices.Contains(c.Args, "-p") || !slices.Contains(c.Args, "2222") || !slices.Contains(c.Args, "-tt") {
		t.Errorf("ssh argv = %v", c.Args)
	}
	remote := c.Args[len(c.Args)-1]
	for _, want := range []string{
		"cd '/srv/repo' && ",
		"K='v with space' ",
		"'claude' '--print' '--system-prompt' 'be terse'",
	} {
		if !strings.Contains(remote, want) {
			t.Errorf("remote missing %q:\n  %s", want, remote)
		}
	}
}

func TestSSHNoWorkDir(t *testing.T) {
	c := SSH{Host: "h"}.Command(ctx, []string{"--print"}, CommandOpts{})
	remote := c.Args[len(c.Args)-1]
	if strings.Contains(remote, "cd ") {
		t.Errorf("should not cd when no workdir: %s", remote)
	}
	if remote != "'claude' '--print'" {
		t.Errorf("remote = %q", remote)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":     "'plain'",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"":          "''",
		"$(rm -rf)": "'$(rm -rf)'", // metachars neutralized by single quotes
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
