package client

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/transport"
)

type termTransport struct {
	transport.Local
	mu         sync.Mutex
	runID      string
	terminated []string
	err        error
}

func (t *termTransport) Command(ctx context.Context, args []string, opt transport.CommandOpts) *exec.Cmd {
	t.mu.Lock()
	t.runID = opt.RunID
	t.mu.Unlock()
	return t.Local.Command(ctx, args, opt)
}

func (t *termTransport) Terminate(_ context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.terminated = append(t.terminated, id)
	return t.err
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// A session whose CLI ignores EOF and SIGTERM-grace must have its remote side
// terminated by Close, with the same tag the command was launched with.
func TestCloseTerminatesRemoteWhenForceKilled(t *testing.T) {
	if testing.Short() {
		t.Skip("waits the 5s graceful window")
	}
	bin := scriptClient(t, "trap '' TERM\nwhile :; do sleep 1; done\n")
	tt := &termTransport{Local: transport.Local{Binary: bin}, err: errors.New("ssh down")}
	logs := &safeBuf{}
	c, err := New(context.Background(), Config{Transport: tt, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.Close()
	if d := time.Since(start); d > 12*time.Second {
		t.Errorf("Close took %v", d)
	}
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if len(tt.terminated) != 1 || tt.terminated[0] != tt.runID || tt.runID == "" {
		t.Errorf("terminated=%v runID=%q", tt.terminated, tt.runID)
	}
	if !bytes.Contains([]byte(logs.String()), []byte("remote teardown failed")) {
		t.Errorf("failure not logged: %s", logs.String())
	}
}

// A proxy that died abnormally may have left the remote running: sweep it.
func TestCloseTerminatesRemoteAfterAbnormalExit(t *testing.T) {
	bin := scriptClient(t, "echo dead >&2\nexit 1\n")
	tt := &termTransport{Local: transport.Local{Binary: bin}}
	c, err := New(context.Background(), Config{Transport: tt})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Wait()
	_ = c.Close()
	tt.mu.Lock()
	defer tt.mu.Unlock()
	if len(tt.terminated) != 1 {
		t.Errorf("terminated=%v", tt.terminated)
	}
}

func TestCloseCleanExitSkipsTerminateAndLocalHasNoTag(t *testing.T) {
	tt := &termTransport{Local: transport.Local{Binary: scriptClient(t, "read line\nexit 0\n")}}
	c, err := New(context.Background(), Config{Transport: tt})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	tt.mu.Lock()
	n := len(tt.terminated)
	tt.mu.Unlock()
	if n != 0 {
		t.Errorf("clean exit should not terminate: %d", n)
	}

	// Plain Local: no Terminator, no tag, Close still works.
	c, err = New(context.Background(), Config{Binary: scriptClient(t, "echo x >&2\nexit 1\n")})
	if err != nil {
		t.Fatal(err)
	}
	if c.runID != "" {
		t.Errorf("Local got runID %q", c.runID)
	}
	_ = c.Wait()
	_ = c.Close()
}
