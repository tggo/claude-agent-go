package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCLINotFound(t *testing.T) {
	r := New(WithBinary("/nonexistent/claude-xyz"))
	_, err := r.Run(context.Background(), Input{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsCLINotFound(err) {
		t.Errorf("expected CLINotFoundError, got %T: %v", err, err)
	}
	var e *CLINotFoundError
	if errors.As(err, &e) && e.Binary != "/nonexistent/claude-xyz" {
		t.Errorf("binary = %q", e.Binary)
	}
}

func TestProcessErrorTyped(t *testing.T) {
	bin := failingClaude(t) // exits 3 with stderr "boom"
	r := New(WithBinary(bin))
	ctx := context.Background()
	for _, run := range []func() error{
		func() error { _, e := r.Run(ctx, Input{Prompt: "x"}); return e },
		func() error { _, e := r.RunJSON(ctx, Input{Prompt: "x"}); return e },
		func() error { _, e := r.RunStream(ctx, Input{Prompt: "x"}, nil); return e },
	} {
		err := run()
		pe, ok := IsProcessError(err)
		if !ok {
			t.Errorf("expected ProcessError, got %T: %v", err, err)
			continue
		}
		if pe.ExitCode != 3 {
			t.Errorf("exit code = %d, want 3", pe.ExitCode)
		}
	}
}

func TestTimeoutErrorTyped(t *testing.T) {
	bin := slowClaude(t)
	r := New(WithBinary(bin), WithTimeout(250*time.Millisecond))
	if _, err := r.Run(context.Background(), Input{Prompt: "x"}); !IsTimeout(err) {
		t.Errorf("Run: expected TimeoutError, got %T: %v", err, err)
	}
	if _, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil); !IsTimeout(err) {
		t.Errorf("RunStream: expected TimeoutError, got %T: %v", err, err)
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{fn: func(s string) { got = append(got, s) }}
	w.Write([]byte("line one\nline "))
	w.Write([]byte("two\nline three\n"))
	w.Write([]byte("partial")) // no newline — buffered, not emitted
	want := []string{"line one", "line two", "line three"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines = %v, want %v", got, want)
	}
}

func TestWithMaxBufferSizeGuard(t *testing.T) {
	// Non-positive values are ignored, keeping the default — no panic.
	r := New(WithMaxBufferSize(-1))
	if r.cfg.MaxBufferSize != maxScanBuf {
		t.Errorf("MaxBufferSize = %d, want default %d", r.cfg.MaxBufferSize, maxScanBuf)
	}
	r2 := New(WithMaxBufferSize(1024))
	if r2.cfg.MaxBufferSize != 1024 {
		t.Errorf("MaxBufferSize = %d, want 1024", r2.cfg.MaxBufferSize)
	}
}

func TestStderrCallback(t *testing.T) {
	// A fake that writes a line to stderr then emits a valid stream result.
	bin := fakeClaudeStderr(t)
	var lines []string
	r := New(WithBinary(bin), WithStderrCallback(func(s string) { lines = append(lines, s) }))
	if _, err := r.RunStream(context.Background(), Input{Prompt: "x"}, nil); err != nil {
		t.Fatalf("RunStream: %v", err)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "warming up") {
		t.Errorf("stderr callback missed lines: %v", lines)
	}
}
