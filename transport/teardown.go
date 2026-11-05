package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Terminator is implemented by transports whose remote process outlives the
// local proxy (ssh, docker exec, docker run). Terminate stops the remote claude
// started with CommandOpts.RunID == id, best-effort and idempotent: a run that
// already exited is not an error. The caller bounds it with ctx.
//
// Local does not implement it — the local process group is already torn down by
// the runner/client.
type Terminator interface {
	Terminate(ctx context.Context, id string) error
}

// TermGrace is how many whole seconds the remote side waits between SIGTERM and
// SIGKILL when terminating.
const TermGrace = 2

// EnvRunID is the environment variable exported into the remote process.
const EnvRunID = "CAG_RUN_ID"

// NewRunID returns a random, shell-safe tag (24 hex chars) identifying one run.
func NewRunID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func pidFile(id string) string { return "/tmp/cag-run-" + id + ".pid" }

// wrapScript runs on the remote side: $1 is the PID file, the rest is the real
// argv. The inner sh records its own PID then execs claude, so the recorded PID
// IS claude's. The outer shell stays as parent only to delete the file on exit
// and propagate the exit status (stdin/stdout/stderr are inherited untouched).
const wrapScript = `f=$1; shift; sh -c 'echo $$ >"$0"; exec "$@"' "$f" "$@"; rc=$?; rm -f "$f"; exit $rc`

// killScript is the remote cleanup: $1 PID file, $2 grace seconds. It walks the
// process tree under the recorded PID using plain ps (pkill is often missing in
// containers), sends TERM, waits up to the grace period, then KILLs whatever is
// left — including descendants orphaned when the parent died first.
const killScript = `f=$1; g=${2:-2}
[ -s "$f" ] || exit 0
p=$(cat "$f")
case $p in ''|*[!0-9]*) rm -f "$f"; exit 0;; esac
kill -0 "$p" 2>/dev/null || { rm -f "$f"; exit 0; }
pt=$(ps -eo pid,ppid 2>/dev/null || ps -o pid,ppid)
kids() { echo "$pt" | while read a b; do
  case $a in ''|*[!0-9]*) continue;; esac
  [ "$b" = "$1" ] && echo "$a"
done; }
tree() { echo "$1"; for c in $(kids "$1"); do tree "$c"; done; }
all=$(tree "$p")
kill -TERM $all 2>/dev/null
i=0
while [ "$i" -lt "$g" ] && kill -0 "$p" 2>/dev/null; do sleep 1; i=$((i+1)); done
kill -KILL $all 2>/dev/null
rm -f "$f"
exit 0`

// wrapArgv returns the remote argv: bin+args as-is when id is empty, else the
// PID-recording wrapper around them. An invalid id is treated as empty.
func wrapArgv(id, bin string, args []string) []string {
	if !validID.MatchString(id) {
		return append([]string{bin}, args...)
	}
	return append([]string{"sh", "-c", wrapScript, "cag-run", pidFile(id), bin}, args...)
}

// killArgv is the argv of the remote kill command for id.
func killArgv(id string) ([]string, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("transport: invalid run id %q", id)
	}
	return []string{"sh", "-c", killScript, "cag-kill", pidFile(id), strconv.Itoa(TermGrace)}, nil
}

// withRunEnv appends CAG_RUN_ID to the extras when a run id is set.
func withRunEnv(opt CommandOpts) []string {
	if !validID.MatchString(opt.RunID) {
		return opt.Env
	}
	return append(append([]string(nil), opt.Env...), EnvRunID+"="+opt.RunID)
}

// runBounded runs cmd to completion, killing it if ctx ends first.
func runBounded(ctx context.Context, cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	if err = cmd.Start(); err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-done
		err = ctx.Err()
	}
	return so.Bytes(), se.Bytes(), err
}

func runTerminate(ctx context.Context, cmd *exec.Cmd, benign string) error {
	_, se, err := runBounded(ctx, cmd)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(se))
	if benign != "" && strings.Contains(msg, benign) {
		return nil
	}
	if msg != "" {
		return fmt.Errorf("transport: terminate: %w: %s", err, msg)
	}
	return fmt.Errorf("transport: terminate: %w", err)
}

// ---- DockerExec ----

func (d DockerExec) dockerBin() string {
	if d.Docker == "" {
		return "docker"
	}
	return d.Docker
}

// TerminateCommand builds `docker exec <ctr> sh -c <killscript>` for id.
func (d DockerExec) TerminateCommand(id string) (*exec.Cmd, error) {
	k, err := killArgv(id)
	if err != nil {
		return nil, err
	}
	a := []string{"exec"}
	if d.User != "" {
		a = append(a, "-u", d.User)
	}
	a = append(a, d.Container)
	a = append(a, k...)
	//nolint:gosec // container comes from trusted config; id is validated.
	return exec.Command(d.dockerBin(), a...), nil
}

// Terminate implements Terminator: SIGTERM, then SIGKILL after TermGrace, the
// claude process tree inside the container.
func (d DockerExec) Terminate(ctx context.Context, id string) error {
	cmd, err := d.TerminateCommand(id)
	if err != nil {
		return err
	}
	return runTerminate(ctx, cmd, "")
}

// ---- DockerRun ----

func (d DockerRun) dockerBin() string {
	if d.Docker == "" {
		return "docker"
	}
	return d.Docker
}

// containerName is the --name for a run: the configured Name, else a generated
// "cag-<id>" so the container can be stopped out of band.
func (d DockerRun) containerName(id string) string {
	if d.Name != "" {
		return d.Name
	}
	if validID.MatchString(id) {
		return "cag-" + id
	}
	return ""
}

// TerminateCommand builds `docker stop -t <grace> <name>` for id.
func (d DockerRun) TerminateCommand(id string) (*exec.Cmd, error) {
	name := d.containerName(id)
	if name == "" {
		return nil, fmt.Errorf("transport: invalid run id %q", id)
	}
	//nolint:gosec // name comes from trusted config or a validated id.
	return exec.Command(d.dockerBin(), "stop", "-t", strconv.Itoa(TermGrace), name), nil
}

// Terminate implements Terminator via `docker stop` (SIGTERM, then SIGKILL);
// --rm removes the container. An already-gone container is not an error.
func (d DockerRun) Terminate(ctx context.Context, id string) error {
	cmd, err := d.TerminateCommand(id)
	if err != nil {
		return err
	}
	return runTerminate(ctx, cmd, "No such container")
}

// ---- SSH ----

func (s SSH) sshBin() string {
	if s.SSHBin == "" {
		return "ssh"
	}
	return s.SSHBin
}

// TerminateCommand builds `ssh host sh -c <killscript> …` for id.
func (s SSH) TerminateCommand(id string) (*exec.Cmd, error) {
	k, err := killArgv(id)
	if err != nil {
		return nil, err
	}
	parts := make([]string, len(k))
	for i, p := range k {
		parts[i] = shellQuote(p)
	}
	//nolint:gosec // host comes from trusted config; remote pieces are quoted.
	return exec.Command(s.sshBin(), s.sshArgs(strings.Join(parts, " "))...), nil
}

// Terminate implements Terminator over a second ssh connection.
func (s SSH) Terminate(ctx context.Context, id string) error {
	cmd, err := s.TerminateCommand(id)
	if err != nil {
		return err
	}
	return runTerminate(ctx, cmd, "")
}

var (
	_ Terminator = DockerExec{}
	_ Terminator = DockerRun{}
	_ Terminator = SSH{}
)
