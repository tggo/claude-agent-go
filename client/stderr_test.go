package client

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scriptClient writes a fake CLI with an arbitrary /bin/sh body.
func scriptClient(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	return path
}

// The point of #2: a caller can capture CLI stderr instead of it vanishing into
// the parent process's own stderr.
func TestStderrFuncCaptures(t *testing.T) {
	bin := scriptClient(t, "echo 'cli diagnostic' >&2\nread line\nexit 0\n")
	var lines []string
	c, err := New(context.Background(), Config{
		Binary:     bin,
		StderrFunc: func(s string) { lines = append(lines, s) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = c.Close()
	if strings.Join(lines, "\n") != "cli diagnostic" {
		t.Errorf("StderrFunc lines = %v, want [cli diagnostic]", lines)
	}
}

// A session that dies must say why, rather than reporting a bare "exit status 1".
func TestExitErrorCarriesStderr(t *testing.T) {
	bin := scriptClient(t, "echo 'auth token expired' >&2\nexit 1\n")
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	werr := c.Wait()
	if werr == nil {
		t.Fatal("expected a non-zero exit error")
	}
	if !strings.Contains(werr.Error(), "auth token expired") {
		t.Errorf("exit error = %q, want the stderr tail", werr)
	}
	// The underlying exec error stays unwrappable for callers that branch on it.
	if !strings.Contains(werr.Error(), "exit status 1") {
		t.Errorf("exit error = %q, want the exit status preserved", werr)
	}
}

// A clean exit reports no error, stderr chatter notwithstanding.
func TestExitErrorNilOnCleanExit(t *testing.T) {
	bin := scriptClient(t, "echo 'just a warning' >&2\nread line\nexit 0\n")
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil on a clean exit", err)
	}
}

// Credentials must not survive into an error string.
func TestExitErrorRedactsTokens(t *testing.T) {
	bin := scriptClient(t, "echo 'fatal: https://x-access-token:ghs_leak@github.com/o/r not found' >&2\nexit 1\n")
	c, err := New(context.Background(), Config{Binary: bin})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	werr := c.Wait()
	if werr == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(werr.Error(), "ghs_leak") {
		t.Errorf("token leaked into the error: %q", werr)
	}
	if !strings.Contains(werr.Error(), "x-access-token:***") {
		t.Errorf("expected redaction marker, got %q", werr)
	}
}

func TestTailBufferBoundsMemory(t *testing.T) {
	b := &tailBuffer{}
	// Write well past the cap in many small writes.
	for i := 0; i < 5000; i++ {
		if _, err := b.Write([]byte("0123456789")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	b.Write([]byte("THE-TAIL"))
	if got := len(b.buf); got > stderrTailMax {
		t.Errorf("retained %d bytes, want <= %d", got, stderrTailMax)
	}
	if s := b.snippet(); !strings.HasSuffix(s, "THE-TAIL") {
		t.Errorf("snippet lost the tail, ends with %q", s[max(0, len(s)-20):])
	}
}

func TestTailBufferReportsFullWrite(t *testing.T) {
	// Under-reporting n would make io.MultiWriter fail with ErrShortWrite and
	// break the os.Stderr passthrough.
	b := &tailBuffer{}
	p := []byte(strings.Repeat("x", stderrTailMax*2))
	n, err := b.Write(p)
	if n != len(p) || err != nil {
		t.Errorf("Write = (%d, %v), want (%d, nil)", n, err, len(p))
	}
}

// captureOSStderr swaps os.Stderr for a pipe, runs fn, and returns what was
// written. Tests using it must not run in parallel — os.Stderr is global.
func captureOSStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 1024)
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- sb.String()
	}()
	fn()
	os.Stderr = orig
	w.Close()
	out := <-done
	r.Close()
	return out
}

// Default behavior must be unchanged for existing callers: no StderrFunc means
// stderr still reaches the parent's stderr.
func TestStderrDefaultsToOSStderr(t *testing.T) {
	bin := scriptClient(t, "echo 'passthrough line' >&2\nread line\nexit 0\n")
	got := captureOSStderr(t, func() {
		c, err := New(context.Background(), Config{Binary: bin})
		if err != nil {
			t.Errorf("New: %v", err)
			return
		}
		_ = c.Close()
	})
	if !strings.Contains(got, "passthrough line") {
		t.Errorf("os.Stderr got %q, want the CLI's stderr forwarded", got)
	}
}

// Setting StderrFunc takes ownership of stderr: it must not also spray the
// parent's stderr, which is the whole point for a caller with a real logger.
func TestStderrFuncSuppressesOSStderr(t *testing.T) {
	bin := scriptClient(t, "echo 'captured line' >&2\nread line\nexit 0\n")
	var lines []string
	got := captureOSStderr(t, func() {
		c, err := New(context.Background(), Config{
			Binary:     bin,
			StderrFunc: func(s string) { lines = append(lines, s) },
		})
		if err != nil {
			t.Errorf("New: %v", err)
			return
		}
		_ = c.Close()
	})
	if strings.Contains(got, "captured line") {
		t.Errorf("os.Stderr got %q, want nothing when StderrFunc is set", got)
	}
	if strings.Join(lines, "\n") != "captured line" {
		t.Errorf("StderrFunc lines = %v", lines)
	}
}
