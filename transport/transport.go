// Package transport decides HOW the claude binary is launched. The runner and
// client own the agent protocol (argv, stdin/stdout piping, teardown); a
// Transport only builds the OS command that actually runs `claude <args>`.
//
// The insight that keeps this small: `docker exec -i` and `ssh host …` are
// themselves local commands that forward stdin/stdout to a remote process. So
// every transport reduces to "build the *exec.Cmd" — Local runs the binary
// directly, Docker prefixes `docker exec`, SSH prefixes `ssh`.
//
// Caveats for remote transports (Docker/SSH):
//   - Teardown kills the LOCAL proxy process. The remote claude may linger;
//     for guaranteed cleanup use `ssh -tt` (so SIGHUP propagates) or stop the
//     container. Process-group teardown applies to the local side only.
//   - In-process tools (the tools package) serve on the host's 127.0.0.1 and
//     are unreachable from inside a container / across SSH without host
//     networking or a reverse tunnel. Network-reachable MCP servers are fine.
//   - WorkDir and any filesystem the agent touches live on the remote side.
package transport

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

// CommandOpts carries the run-specific bits a transport must apply: the working
// directory the claude process should use, and EXTRA environment variables
// ("KEY=VALUE"). Env is just the extras — Local merges them with the host
// environment; Docker/SSH forward only these, never the whole host env.
type CommandOpts struct {
	WorkDir string
	Env     []string
}

// Transport builds the command that runs claude with the given args. It must
// not bind the context for process killing — the caller owns teardown — but it
// receives ctx so it can attach it to the returned *exec.Cmd's lifetime if it
// needs to (the bundled transports do not).
type Transport interface {
	Command(ctx context.Context, args []string, opt CommandOpts) *exec.Cmd
}

// ---- Local ----

// Local runs the binary directly on this host (the default).
type Local struct {
	Binary string // default "claude"
}

func (l Local) Command(ctx context.Context, args []string, opt CommandOpts) *exec.Cmd {
	bin := l.Binary
	if bin == "" {
		bin = "claude"
	}
	//nolint:gosec // binary comes from trusted config.
	cmd := exec.Command(bin, args...)
	cmd.Dir = opt.WorkDir
	cmd.Env = append(os.Environ(), opt.Env...)
	return cmd
}

// ---- Docker ----

// DockerExec runs claude inside an already-running container via `docker exec`.
type DockerExec struct {
	Container string // container name or id (required)
	Binary    string // claude path inside the container, default "claude"
	User      string // optional --user
	Docker    string // docker binary, default "docker"
}

func (d DockerExec) Command(ctx context.Context, args []string, opt CommandOpts) *exec.Cmd {
	docker := d.Docker
	if docker == "" {
		docker = "docker"
	}
	bin := d.Binary
	if bin == "" {
		bin = "claude"
	}
	a := []string{"exec", "-i"}
	if opt.WorkDir != "" {
		a = append(a, "-w", opt.WorkDir)
	}
	if d.User != "" {
		a = append(a, "-u", d.User)
	}
	for _, e := range opt.Env {
		a = append(a, "-e", e)
	}
	a = append(a, d.Container, bin)
	a = append(a, args...)
	//nolint:gosec // container/args come from trusted config.
	return exec.Command(docker, a...)
}

// ---- SSH ----

// SSH runs claude on a remote host via ssh. Stdin/stdout are forwarded by ssh,
// so the prompt (sent over stdin) and stream-json output work unchanged.
type SSH struct {
	Host    string   // user@host (required)
	Binary  string   // claude path on the remote, default "claude"
	Port    string   // optional -p
	SSHBin  string   // ssh binary, default "ssh"
	Options []string // extra ssh flags, e.g. {"-tt"} or {"-i","/key"}
}

func (s SSH) Command(ctx context.Context, args []string, opt CommandOpts) *exec.Cmd {
	sshBin := s.SSHBin
	if sshBin == "" {
		sshBin = "ssh"
	}
	bin := s.Binary
	if bin == "" {
		bin = "claude"
	}

	// Build the remote command string: [cd <wd> &&] [K=V …] claude <args…>,
	// shell-quoting every dynamic piece for the remote shell.
	var b strings.Builder
	if opt.WorkDir != "" {
		b.WriteString("cd ")
		b.WriteString(shellQuote(opt.WorkDir))
		b.WriteString(" && ")
	}
	for _, e := range opt.Env {
		if k, v, ok := strings.Cut(e, "="); ok {
			b.WriteString(k)
			b.WriteString("=")
			b.WriteString(shellQuote(v))
			b.WriteString(" ")
		}
	}
	b.WriteString(shellQuote(bin))
	for _, arg := range args {
		b.WriteString(" ")
		b.WriteString(shellQuote(arg))
	}

	a := []string{}
	if s.Port != "" {
		a = append(a, "-p", s.Port)
	}
	a = append(a, s.Options...)
	a = append(a, s.Host, b.String())
	//nolint:gosec // host/args come from trusted config; remote pieces are quoted.
	return exec.Command(sshBin, a...)
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it
// survives one round of POSIX shell word-splitting on the remote host.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
