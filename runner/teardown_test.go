package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tggo/claude-agent-go/claudecli"
	"github.com/tggo/claude-agent-go/transport"
)

// termTransport wraps Local, records the run IDs it was given and Terminate
// calls, and can make Terminate fail or block.
type termTransport struct {
	transport.Local
	mu         sync.Mutex
	cmdIDs     []string
	terminated []string
	err        error
	block      bool
	gotDL      bool
}

func (t *termTransport) Command(ctx context.Context, args []string, opt transport.CommandOpts) *exec.Cmd {
	t.mu.Lock()
	t.cmdIDs = append(t.cmdIDs, opt.RunID)
	t.mu.Unlock()
	return t.Local.Command(ctx, args, opt)
}

func (t *termTransport) Terminate(ctx context.Context, id string) error {
	t.mu.Lock()
	t.terminated = append(t.terminated, id)
	t.mu.Unlock()
	if t.block {
		_, dl := ctx.Deadline()
		t.mu.Lock()
		t.gotDL = dl
		t.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	return t.err
}

func (t *termTransport) snapshot() (cmdIDs, terminated []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.cmdIDs...), append([]string(nil), t.terminated...)
}

type vetoPlugin struct{}

func (vetoPlugin) Name() string { return "veto" }
func (vetoPlugin) OnEvent(context.Context, claudecli.StreamEvent) error {
	return errors.New("no")
}

func TestTeardownTerminatesOnTimeoutAllModes(t *testing.T) {
	bin := shFake(t, "sleep 20")
	for name, run := range map[string]func(*Runner) error{
		"plain": func(r *Runner) error { _, err := r.Run(context.Background(), Input{Prompt: "x"}); return err },
		"json":  func(r *Runner) error { _, err := r.RunJSON(context.Background(), Input{Prompt: "x"}); return err },
		"stream": func(r *Runner) error {
			_, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			tt := &termTransport{Local: transport.Local{Binary: bin}}
			r := New(WithTransport(tt), WithTimeout(300*time.Millisecond))
			var te *TimeoutError
			require.ErrorAs(t, run(r), &te)
			ids, term := tt.snapshot()
			require.Len(t, ids, 1)
			assert.Regexp(t, `^[0-9a-f]{24}$`, ids[0])
			assert.Equal(t, ids, term, "Terminate must get the same id the command was tagged with")
		})
	}
}

func TestTeardownTerminatesOnCancel(t *testing.T) {
	tt := &termTransport{Local: transport.Local{Binary: shFake(t, "sleep 20")}}
	r := New(WithTransport(tt), WithTimeout(time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := r.RunJSON(ctx, Input{Prompt: "x"})
	require.ErrorIs(t, err, context.Canceled)
	_, term := tt.snapshot()
	assert.Len(t, term, 1)
}

func TestTeardownTerminatesOnAbort(t *testing.T) {
	t.Run("line too long", func(t *testing.T) {
		tt := &termTransport{Local: transport.Local{Binary: shFake(t, initLine+"\nhead -c 100000 /dev/zero | tr '\\0' x; echo\nsleep 20")}}
		r := New(WithTransport(tt), WithMaxBufferSize(1000), WithTimeout(time.Minute))
		_, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
		var lt *LineTooLongError
		require.ErrorAs(t, err, &lt)
		_, term := tt.snapshot()
		assert.Len(t, term, 1)
	})
	t.Run("plugin", func(t *testing.T) {
		tt := &termTransport{Local: transport.Local{Binary: shFake(t, initLine+"\nsleep 20")}}
		r := New(WithTransport(tt), WithPlugins(vetoPlugin{}), WithTimeout(time.Minute))
		_, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil)
		var pa *PluginAbortError
		require.ErrorAs(t, err, &pa)
		_, term := tt.snapshot()
		assert.Len(t, term, 1)
	})
}

func TestNoTerminateOnSuccess(t *testing.T) {
	tt := &termTransport{Local: transport.Local{Binary: shFake(t, "echo hi")}}
	r := New(WithTransport(tt))
	_, err := r.Run(context.Background(), Input{Prompt: "x"})
	require.NoError(t, err)
	ids, term := tt.snapshot()
	assert.Len(t, ids, 1)
	assert.Empty(t, term)
}

func TestLocalGetsNoRunID(t *testing.T) {
	var _ transport.Transport = transport.Local{}
	_, ok := any(transport.Local{}).(transport.Terminator)
	assert.False(t, ok)
	r := New(WithBinary(shFake(t, "env | grep -c CAG_RUN_ID || true")))
	res, err := r.Run(context.Background(), Input{Prompt: "x"})
	require.NoError(t, err)
	assert.Equal(t, "0", res.Response)
}

func TestTerminateFailureIsLoggedNotFatal(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	lg := slog.New(slog.NewTextHandler(&lockedWriter{&mu, &buf}, nil))
	tt := &termTransport{Local: transport.Local{Binary: shFake(t, "sleep 20")}, err: errors.New("ssh down")}
	r := New(WithTransport(tt), WithTimeout(200*time.Millisecond), WithLogger(lg))
	_, err := r.Run(context.Background(), Input{Prompt: "x"})
	var te *TimeoutError
	require.ErrorAs(t, err, &te)
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, buf.String(), "remote teardown failed")
	assert.Contains(t, buf.String(), "ssh down")
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestTerminateIsBounded(t *testing.T) {
	old := terminateTimeout
	terminateTimeout = 300 * time.Millisecond
	defer func() { terminateTimeout = old }()
	tt := &termTransport{Local: transport.Local{Binary: shFake(t, "sleep 20")}, block: true}
	r := New(WithTransport(tt), WithTimeout(200*time.Millisecond))
	start := time.Now()
	_, err := r.Run(context.Background(), Input{Prompt: "x"})
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	tt.mu.Lock()
	assert.True(t, tt.gotDL, "Terminate ctx must carry a deadline")
	tt.mu.Unlock()
}

// End to end through a fake ssh that executes the remote command in a local
// shell: a claude that ignores SIGTERM and has a child must be gone after the
// run times out, and its pid file removed.
func TestSSHRunTimeoutKillsRemoteProcess(t *testing.T) {
	dir := t.TempDir()
	sshBin := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(sshBin, []byte("#!/bin/sh\nfor a; do last=$a; done\nexec sh -c \"$last\"\n"), 0o755))
	pidOut := filepath.Join(dir, "claude.pid")
	claude := shFake(t, "trap '' TERM\necho $$ > "+pidOut+"\nsleep 60 &\nwhile :; do sleep 1; done")

	tr := transport.SSH{Host: "h", SSHBin: sshBin, Binary: claude}
	r := New(WithTransport(tr), WithTimeout(time.Second))
	start := time.Now()
	_, err := r.RunJSON(context.Background(), Input{Prompt: "x"})
	var te *TimeoutError
	require.ErrorAs(t, err, &te)
	assert.Less(t, time.Since(start), 12*time.Second)

	b, rerr := os.ReadFile(pidOut)
	require.NoError(t, rerr)
	pid := strings.TrimSpace(string(b))
	assert.Error(t, exec.Command("kill", "-0", pid).Run(), "remote claude (pid %s) survived teardown", pid)
	left, _ := filepath.Glob("/tmp/cag-run-*.pid")
	for _, f := range left {
		if c, _ := os.ReadFile(f); strings.TrimSpace(string(c)) == pid {
			t.Errorf("pid file %s left behind", f)
		}
	}
}

func fakeVersionBin(t *testing.T, ver string) string {
	return shFake(t, `case "$1" in --version) echo "`+ver+` (Claude Code)";; auth) echo '{"loggedIn":true,"authMethod":"claude.ai"}';; *) echo ran;; esac`)
}

func TestRunnerPreflight(t *testing.T) {
	info, err := New(WithBinary(fakeVersionBin(t, "2.1.318"))).Preflight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "2.1.318", info.Version)
	assert.True(t, info.AuthChecked)

	_, err = New(WithBinary(fakeVersionBin(t, "2.1.6"))).Preflight(context.Background())
	assert.ErrorIs(t, err, ErrCLITooOld)

	_, err = New(WithBinary("/no/such/claude")).Preflight(context.Background())
	assert.ErrorIs(t, err, ErrClaudeNotFound)

	_, err = New(WithBinary(shFake(t, `echo '{"loggedIn":false}'; case "$1" in --version) echo 2.1.318;; esac`))).Preflight(context.Background())
	assert.ErrorIs(t, err, ErrNotAuthenticated)
}

func TestWithPreflightGatesFirstRun(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "n")
	bin := shFake(t, `case "$1" in --version) echo x >> `+counter+`; echo "2.1.6";; *) echo ran;; esac`)
	r := New(WithBinary(bin), WithPreflight(true))
	for i := 0; i < 3; i++ {
		_, err := r.Run(context.Background(), Input{Prompt: "x"})
		require.ErrorIs(t, err, ErrCLITooOld)
	}
	b, _ := os.ReadFile(counter)
	assert.Equal(t, 1, strings.Count(string(b), "x"), "preflight must run once and be cached")

	ok := New(WithBinary(fakeVersionBin(t, "2.1.318")), WithPreflight(true))
	res, err := ok.Run(context.Background(), Input{Prompt: "x"})
	require.NoError(t, err)
	assert.Equal(t, "ran", res.Response)

	off := New(WithBinary(fakeVersionBin(t, "2.1.6")))
	_, err = off.Run(context.Background(), Input{Prompt: "x"})
	require.NoError(t, err, "preflight is opt-in")
}
