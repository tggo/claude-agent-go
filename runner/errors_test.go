package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// scriptClaude writes a fake binary with an arbitrary /bin/sh body.
func scriptClaude(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary uses /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

// The reported production failure: the CLI dies with exit 1 having written
// nothing to either stream. The error must still say something.
func TestProcessErrorSilentDeath(t *testing.T) {
	bin := scriptClaude(t, "exit 1\n")
	r := New(WithBinary(bin))
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"Run":       func() error { _, e := r.Run(ctx, Input{Prompt: "x"}); return e },
		"RunJSON":   func() error { _, e := r.RunJSON(ctx, Input{Prompt: "x"}); return e },
		"RunStream": func() error { _, e := r.RunStream(ctx, Input{Prompt: "x"}, nil); return e },
	} {
		t.Run(name, func(t *testing.T) {
			pe, ok := IsProcessError(run())
			if !ok {
				t.Fatalf("expected ProcessError")
			}
			if pe.ProcessState == nil {
				t.Error("ProcessState not captured")
			}
			// The bug: the message used to end at the colon with nothing after it.
			msg := pe.Error()
			if strings.HasSuffix(msg, ": ") || !strings.Contains(msg, "no output") {
				t.Errorf("uninformative message: %q", msg)
			}
			if rss, ok := pe.MaxRSSBytes(); ok && rss <= 0 {
				t.Errorf("peak rss reported but non-positive: %d", rss)
			}
		})
	}
}

// When stderr is empty, the tail of stdout must survive into the error — it is
// the only remaining clue (a stream-json error event, a truncated JSON blob).
func TestProcessErrorFallsBackToStdout(t *testing.T) {
	bin := scriptClaude(t, "echo 'partial output, then death'\nexit 1\n")
	r := New(WithBinary(bin))
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"Run":       func() error { _, e := r.Run(ctx, Input{Prompt: "x"}); return e },
		"RunJSON":   func() error { _, e := r.RunJSON(ctx, Input{Prompt: "x"}); return e },
		"RunStream": func() error { _, e := r.RunStream(ctx, Input{Prompt: "x"}, nil); return e },
	} {
		t.Run(name, func(t *testing.T) {
			pe, ok := IsProcessError(run())
			if !ok {
				t.Fatalf("expected ProcessError")
			}
			if !strings.Contains(pe.Stdout, "partial output, then death") {
				t.Errorf("Stdout = %q, want the captured stdout", pe.Stdout)
			}
			if !strings.Contains(pe.Error(), "partial output, then death") {
				t.Errorf("stdout did not reach the message: %q", pe.Error())
			}
		})
	}
}

// Run used to file stdout under ProcessError.Stderr, so real stderr was lost.
func TestProcessErrorStreamsNotSwapped(t *testing.T) {
	bin := scriptClaude(t, "echo 'this is stdout'\necho 'this is stderr' >&2\nexit 3\n")
	r := New(WithBinary(bin))
	ctx := context.Background()

	for name, run := range map[string]func() error{
		"Run":     func() error { _, e := r.Run(ctx, Input{Prompt: "x"}); return e },
		"RunJSON": func() error { _, e := r.RunJSON(ctx, Input{Prompt: "x"}); return e },
	} {
		t.Run(name, func(t *testing.T) {
			pe, ok := IsProcessError(run())
			if !ok {
				t.Fatalf("expected ProcessError")
			}
			if !strings.Contains(pe.Stderr, "this is stderr") {
				t.Errorf("Stderr = %q, want the stderr stream", pe.Stderr)
			}
			if strings.Contains(pe.Stderr, "this is stdout") {
				t.Errorf("Stderr carries stdout content: %q", pe.Stderr)
			}
			if !strings.Contains(pe.Stdout, "this is stdout") {
				t.Errorf("Stdout = %q, want the stdout stream", pe.Stdout)
			}
			// stderr wins the message when both are present.
			if !strings.Contains(pe.Error(), "this is stderr") {
				t.Errorf("message = %q, want stderr", pe.Error())
			}
		})
	}
}

// A signal-killed process (what an OOM kill often looks like) reports the
// signal rather than a bare exit code.
func TestProcessErrorSignalled(t *testing.T) {
	bin := scriptClaude(t, "kill -9 $$\n")
	r := New(WithBinary(bin))
	pe, ok := IsProcessError(func() error { _, e := r.RunJSON(context.Background(), Input{Prompt: "x"}); return e }())
	if !ok {
		t.Fatalf("expected ProcessError")
	}
	if !strings.Contains(pe.Error(), "signal:") {
		t.Errorf("message = %q, want the signal", pe.Error())
	}
}

// The stderr callback must fire in Run/RunJSON, not just RunStream — a run that
// dies should already have delivered its stderr.
func TestStderrCallbackAllModes(t *testing.T) {
	bin := scriptClaude(t, "echo 'warming up' >&2\nexit 1\n")
	ctx := context.Background()

	for name, run := range map[string]func(*Runner) error{
		"Run":     func(r *Runner) error { _, e := r.Run(ctx, Input{Prompt: "x"}); return e },
		"RunJSON": func(r *Runner) error { _, e := r.RunJSON(ctx, Input{Prompt: "x"}); return e },
	} {
		t.Run(name, func(t *testing.T) {
			var lines []string
			r := New(WithBinary(bin), WithStderrCallback(func(s string) { lines = append(lines, s) }))
			_ = run(r)
			if strings.Join(lines, "\n") != "warming up" {
				t.Errorf("stderr callback lines = %v, want [warming up]", lines)
			}
		})
	}
}

func TestSanitizeTail(t *testing.T) {
	// Keeps the end, where the failure is.
	long := strings.Repeat("a", 3000) + "THE-END"
	got := sanitizeTail([]byte(long))
	if !strings.Contains(got, "THE-END") {
		t.Error("tail dropped the end of the output")
	}
	if !strings.HasPrefix(got, "(truncated) ...") {
		t.Errorf("truncation not marked: %q", got[:20])
	}
	// Short output passes through whole.
	if got := sanitizeTail([]byte("short")); got != "short" {
		t.Errorf("sanitizeTail(short) = %q", got)
	}
	// Tokens are redacted here too, not just in sanitizeOutput.
	if got := sanitizeTail([]byte("https://x-access-token:secret@github.com/o/r")); strings.Contains(got, "secret") {
		t.Errorf("token leaked: %q", got)
	}
}

func TestTailBytes(t *testing.T) {
	lines := [][]byte{[]byte("one"), []byte("two"), []byte("three")}
	if got := string(tailBytes(lines, 2)); got != "two\nthree" {
		t.Errorf("tailBytes = %q, want last two lines", got)
	}
	if got := string(tailBytes(lines, 10)); got != "one\ntwo\nthree" {
		t.Errorf("tailBytes = %q, want all lines", got)
	}
	if got := string(tailBytes(nil, 5)); got != "" {
		t.Errorf("tailBytes(nil) = %q", got)
	}
}

// A ProcessError built without a process (never ran) must not panic.
func TestProcessErrorNoState(t *testing.T) {
	pe := &ProcessError{ExitCode: 1}
	if _, ok := pe.MaxRSSBytes(); ok {
		t.Error("MaxRSSBytes should report unavailable without a ProcessState")
	}
	if !strings.Contains(pe.Error(), "no output") {
		t.Errorf("message = %q", pe.Error())
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
