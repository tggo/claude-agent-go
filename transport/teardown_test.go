package transport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func skipWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
}

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	skipWindows(t)
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeLauncher logs its argv (one arg per line, records separated by "--") to
// log, then behaves per body.
func fakeLauncher(t *testing.T, name, body string) (bin, log string) {
	t.Helper()
	log = filepath.Join(t.TempDir(), name+".log")
	bin = writeScript(t, name, `for a in "$@"; do printf '%s\n' "$a" >>`+log+`; done; echo -- >>`+log+"\n"+body)
	return bin, log
}

func readLog(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestNewRunID(t *testing.T) {
	a, b := NewRunID(), NewRunID()
	if a == b || !validID.MatchString(a) || len(a) != 24 {
		t.Fatalf("ids %q %q", a, b)
	}
}

func TestRunIDWiring(t *testing.T) {
	id := "abc123"
	pf := pidFile(id)

	ssh := SSH{Host: "u@h"}.Command(ctx, []string{"-p", "hi there"}, CommandOpts{WorkDir: "/w", Env: []string{"A=b"}, RunID: id})
	remote := ssh.Args[len(ssh.Args)-1]
	for _, want := range []string{"cd '/w' && ", "CAG_RUN_ID='abc123'", "A='b'", "'sh' '-c'", "'cag-run' '" + pf + "' 'claude' '-p' 'hi there'"} {
		if !strings.Contains(remote, want) {
			t.Errorf("ssh remote missing %q in %s", want, remote)
		}
	}

	de := DockerExec{Container: "c"}.Command(ctx, []string{"-p"}, CommandOpts{RunID: id})
	j := strings.Join(de.Args, "\x00")
	if !strings.Contains(j, "-e\x00CAG_RUN_ID\x00c\x00sh\x00-c\x00"+wrapScript+"\x00cag-run\x00"+pf+"\x00claude\x00-p") {
		t.Errorf("docker exec args = %q", de.Args)
	}

	dr := DockerRun{Image: "img"}.Command(ctx, nil, CommandOpts{RunID: id})
	if !strings.Contains(strings.Join(dr.Args, " "), "--name cag-abc123") || !strings.Contains(strings.Join(dr.Args, " "), "-e CAG_RUN_ID") || !slices.Contains(dr.Env, "CAG_RUN_ID=abc123") {
		t.Errorf("docker run args = %q", dr.Args)
	}
	dr = DockerRun{Image: "img", Name: "mine"}.Command(ctx, nil, CommandOpts{RunID: id})
	if !strings.Contains(strings.Join(dr.Args, " "), "--name mine") {
		t.Errorf("explicit name lost: %q", dr.Args)
	}

	// Invalid id: legacy argv, no env.
	de = DockerExec{Container: "c"}.Command(ctx, []string{"-p"}, CommandOpts{RunID: "bad id;rm"})
	if strings.Join(de.Args, " ") != "docker exec -i c claude -p" {
		t.Errorf("invalid id should be ignored: %q", de.Args)
	}
	dr = DockerRun{Image: "img"}.Command(ctx, nil, CommandOpts{})
	if strings.Contains(strings.Join(dr.Args, " "), "--name") {
		t.Errorf("no id, no name expected: %q", dr.Args)
	}
}

// The real wrapper + kill scripts, executed locally with a claude that traps
// SIGTERM and spawns a child: Terminate must still end both within grace.
func TestWrapAndKillScriptsEndToEnd(t *testing.T) {
	skipWindows(t)
	id := NewRunID()
	pf := pidFile(id)
	defer os.Remove(pf)
	childPid := filepath.Join(t.TempDir(), "child.pid")
	claude := writeScript(t, "claude", `trap '' TERM
sleep 60 &
echo $! > `+childPid+`
while :; do sleep 1; done`)

	argv := wrapArgv(id, claude, []string{"--x"})
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	waitFor(t, func() bool { b, _ := os.ReadFile(childPid); return len(b) > 0 })
	b, err := os.ReadFile(pf)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		t.Fatalf("pid file not written: %v %q", err, b)
	}

	k, _ := killArgv(id)
	start := time.Now()
	if out, err := exec.Command(k[0], k[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("kill script: %v %s", err, out)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("kill took %v", d)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("wrapped claude still alive after kill")
	}
	cp, _ := os.ReadFile(childPid)
	if exec.Command("kill", "-0", strings.TrimSpace(string(cp))).Run() == nil {
		t.Error("descendant survived")
	}
	if _, err := os.Stat(pf); err == nil {
		t.Error("pid file not removed")
	}
	// Idempotent: second kill with no pid file is a no-op.
	if out, err := exec.Command(k[0], k[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("second kill: %v %s", err, out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestWrapperPropagatesExitAndCleansUp(t *testing.T) {
	skipWindows(t)
	id := NewRunID()
	defer os.Remove(pidFile(id))
	claude := writeScript(t, "claude", `cat; exit 7`)
	argv := wrapArgv(id, claude, nil)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader("in")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if !asExit(err, &ee) || ee.ExitCode() != 7 || string(out) != "in" {
		t.Fatalf("exit/out = %v %q", err, out)
	}
	if _, err := os.Stat(pidFile(id)); err == nil {
		t.Error("pid file left behind after normal exit")
	}
}

func asExit(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	*target = e
	return ok
}

func TestKillScriptStalePidFile(t *testing.T) {
	skipWindows(t)
	for _, content := range []string{"999999", "garbage"} {
		id := NewRunID()
		_ = os.WriteFile(pidFile(id), []byte(content), 0o644)
		k, _ := killArgv(id)
		if out, err := exec.Command(k[0], k[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		if _, err := os.Stat(pidFile(id)); err == nil {
			t.Errorf("stale pid file %q not removed", content)
			os.Remove(pidFile(id))
		}
	}
}

// SSH.Terminate issues `ssh host 'sh' '-c' <kill script> 'cag-kill' <pidfile> 2`.
func TestSSHTerminateCommand(t *testing.T) {
	bin, log := fakeLauncher(t, "ssh", "exit 0")
	s := SSH{Host: "u@h", Port: "2222", SSHBin: bin, Options: []string{"-o", "BatchMode=yes"}}
	if err := s.Terminate(ctx, "run42"); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, log)
	for _, want := range []string{"-p\n2222\n-o\nBatchMode=yes\nu@h\n", "'cag-kill' '/tmp/cag-run-run42.pid' '2'", "kill -TERM", "kill -KILL"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
}

// Fake ssh that executes the remote command locally proves the quoting survives
// a real shell round trip: launch via SSH.Command, terminate via SSH.Terminate.
func TestSSHRoundTripLocalShell(t *testing.T) {
	skipWindows(t)
	sshBin := writeScript(t, "ssh", `for a; do last=$a; done; exec sh -c "$last"`)
	claude := writeScript(t, "claude", `trap '' TERM; while :; do sleep 1; done`)
	s := SSH{Host: "h", SSHBin: sshBin, Binary: claude}
	id := NewRunID()
	defer os.Remove(pidFile(id))
	cmd := s.Command(ctx, []string{"--print"}, CommandOpts{RunID: id})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	waitFor(t, func() bool { _, err := os.Stat(pidFile(id)); return err == nil })
	if err := s.Terminate(ctx, id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(6 * time.Second):
		t.Fatal("remote claude survived Terminate")
	}
}

func TestTerminateBoundedByContext(t *testing.T) {
	bin, _ := fakeLauncher(t, "ssh", "sleep 30")
	c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := SSH{Host: "h", SSHBin: bin}.Terminate(c, "abc")
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestTerminateErrorsIncludeStderr(t *testing.T) {
	bin, _ := fakeLauncher(t, "ssh", "echo 'boom' >&2; exit 255")
	err := SSH{Host: "h", SSHBin: bin}.Terminate(ctx, "abc")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	bin, _ = fakeLauncher(t, "ssh2", "exit 255")
	if err := (SSH{Host: "h", SSHBin: bin}).Terminate(ctx, "abc"); err == nil {
		t.Fatal("want error")
	}
	if err := (SSH{Host: "h", SSHBin: "/nonexistent/ssh"}).Terminate(ctx, "abc"); err == nil {
		t.Fatal("want start error")
	}
}

func TestDockerExecTerminate(t *testing.T) {
	bin, log := fakeLauncher(t, "docker", "exit 0")
	if err := (DockerExec{Container: "ctr", User: "node", Docker: bin}).Terminate(ctx, "xyz"); err != nil {
		t.Fatal(err)
	}
	got := readLog(t, log)
	if !strings.HasPrefix(got, "exec\n-u\nnode\nctr\nsh\n-c\n") || !strings.Contains(got, "cag-kill\n/tmp/cag-run-xyz.pid\n2\n") {
		t.Errorf("log:\n%s", got)
	}
}

func TestDockerRunTerminate(t *testing.T) {
	bin, log := fakeLauncher(t, "docker", "exit 0")
	if err := (DockerRun{Image: "i", Docker: bin}).Terminate(ctx, "xyz"); err != nil {
		t.Fatal(err)
	}
	if got := readLog(t, log); got != "stop\n-t\n2\ncag-xyz\n--\n" {
		t.Errorf("log %q", got)
	}
	// Explicit Name wins.
	_ = os.Remove(log)
	_ = DockerRun{Image: "i", Docker: bin, Name: "fixed"}.Terminate(ctx, "xyz")
	if got := readLog(t, log); !strings.Contains(got, "fixed") {
		t.Errorf("log %q", got)
	}
	// Already gone: benign.
	gone, _ := fakeLauncher(t, "docker2", "echo 'Error response from daemon: No such container: cag-xyz' >&2; exit 1")
	if err := (DockerRun{Image: "i", Docker: gone}).Terminate(ctx, "xyz"); err != nil {
		t.Errorf("missing container should be benign: %v", err)
	}
	bad, _ := fakeLauncher(t, "docker3", "exit 1")
	if err := (DockerRun{Image: "i", Docker: bad}).Terminate(ctx, "xyz"); err == nil {
		t.Error("want error")
	}
}

func TestTerminateInvalidID(t *testing.T) {
	for name, err := range map[string]error{
		"ssh":  SSH{Host: "h"}.Terminate(ctx, "a b;c"),
		"exec": DockerExec{Container: "c"}.Terminate(ctx, ""),
		"run":  DockerRun{Image: "i"}.Terminate(ctx, "../x"),
	} {
		if err == nil {
			t.Errorf("%s: want error for invalid id", name)
		}
	}
}

func TestDefaultBinaries(t *testing.T) {
	if (DockerExec{}).dockerBin() != "docker" || (DockerRun{}).dockerBin() != "docker" || (SSH{}).sshBin() != "ssh" {
		t.Error("defaults")
	}
}
