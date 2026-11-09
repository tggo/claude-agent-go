package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/claudecli"
)

// fakeVerbose emits 300 assistant lines per user turn and no result, then
// keeps reading stdin, like a verbose turn the caller walks away from.
func fakeVerbose(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in *control_request*) continue ;; esac
  i=0
  while [ $i -lt 300 ]; do
    printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}'
    i=$((i+1))
  done
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// An abandoned Query with more than 64 unread events must not leave Close (or
// Wait) blocked: the read loop used to block on the full events channel and
// never close c.done.
func TestCloseAfterAbandonedVerboseQuery(t *testing.T) {
	c, err := New(context.Background(), Config{Binary: fakeVerbose(t)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = c.Query(ctx, "go", func(claudecli.StreamEvent) { cancel() })
	if err == nil {
		t.Fatal("Query: want context error")
	}
	time.Sleep(500 * time.Millisecond) // let the CLI fill the old 64-slot buffer

	closed := make(chan struct{})
	go func() { _ = c.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close blocked after an abandoned Query")
	}
	waited := make(chan struct{})
	go func() { _ = c.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("Wait blocked after Close")
	}
}

// Without Close, Wait must return once the process exits even if nobody read
// the turn's events.
func TestWaitWithUnreadEvents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
i=0
while [ $i -lt 300 ]; do
  printf '%s\n' '{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"x"}]}}'
  i=$((i+1))
done
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := New(context.Background(), Config{Binary: path})
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() { _ = c.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait blocked on unread events")
	}
	_ = c.Close()
}
