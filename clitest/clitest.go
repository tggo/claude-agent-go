// Package clitest builds fake `claude` binaries for tests of code that drives
// the CLI through this SDK (runner, client, graph agents). A fake is a tiny
// /bin/sh script in t.TempDir() that prints canned stream-json, optionally
// writes to stderr, sleeps, and exits with a chosen code. It records its argv
// (and, on request, stdin) so a test can assert on how the SDK invoked it:
//
//	bin := clitest.FakeCLI(t,
//		clitest.Init("sess-1"),
//		clitest.ToolUse("t1", "get_menu", `{}`),
//		clitest.ToolResult("t1", "margherita, pepperoni"),
//		clitest.Text("Two options."),
//		clitest.Result("Two options.", 0.002),
//	)
//	res, err := runner.New(runner.WithBinary(bin)).RunStream(ctx, in, nil)
//	args := clitest.Args(t, bin) // flags the SDK passed
//
// Every event after Init carries Init's session id, so streams built here
// satisfy claudecli.CheckInvariants. Steps run in order; the script exits 0
// at the end unless an ExitCode step says otherwise.
//
// The fakes need /bin/sh: they are Unix-only (macOS, Linux, WSL) and the
// helpers skip the test on Windows.
package clitest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tggo/claude-agent-go/recording"
)

// Step is one action of a fake CLI's script. Build them with Init, Text,
// ToolUse, ToolResult, Result, Raw, Stderr, Sleep, ExitCode and RecordStdin.
type Step interface {
	// shell appends the step to the script. dir is the fake's private
	// directory; st carries state shared across steps.
	shell(b *strings.Builder, st *state)
}

type state struct {
	dir     string
	session string
}

const (
	argsFile  = "args"
	stdinFile = "stdin"
)

// FakeCLI writes an executable fake `claude` to t.TempDir() running script and
// returns its path, ready for runner.WithBinary (or client.Config.Binary). The
// test is skipped on Windows.
func FakeCLI(t testing.TB, script ...Step) string {
	t.Helper()
	skipWindows(t)
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(build(dir, script)), 0o755); err != nil {
		t.Fatalf("clitest: write fake binary: %v", err)
	}
	return bin
}

// FromRecording writes a fake CLI that replays the body of a recording
// (recording.ReplayLines) and then runs any extra steps. Unlike
// recording.ReplayTransport it needs no sed: the recording is stripped of its
// header and timing field in Go and stored next to the script, which just
// cats it. Args are ignored, as with the transport: the recording decides the
// output. stdin is drained so the runner's prompt write never fails.
func FromRecording(t testing.TB, path string, then ...Step) string {
	t.Helper()
	skipWindows(t)
	lines, err := recording.ReplayLines(path)
	if err != nil {
		t.Fatalf("clitest: %v", err)
	}
	dir := t.TempDir()
	body := filepath.Join(dir, "body.jsonl")
	if err := os.WriteFile(body, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatalf("clitest: %v", err)
	}
	steps := append([]Step{fileStep(body)}, then...)
	steps = append(steps, drainStdin{})
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(build(dir, steps)), 0o755); err != nil {
		t.Fatalf("clitest: write fake binary: %v", err)
	}
	return bin
}

func skipWindows(t testing.TB) {
	if runtime.GOOS == "windows" {
		t.Skip("clitest fakes use /bin/sh")
	}
}

func build(dir string, steps []Step) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "printf '%%s\\0' \"$@\" > %s\n", quote(filepath.Join(dir, argsFile)))
	st := &state{dir: dir}
	exit := 0
	for _, s := range steps {
		if e, ok := s.(exitStep); ok {
			exit = int(e)
		}
		s.shell(&b, st)
	}
	fmt.Fprintf(&b, "exit %d\n", exit)
	return b.String()
}

// quote single-quotes s for /bin/sh.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Args returns the argv (without the program name) the fake was last invoked
// with.
func Args(t testing.TB, bin string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), argsFile))
	if err != nil {
		t.Fatalf("clitest: fake was never run: %v", err)
	}
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// Stdin returns what the fake received on stdin. The fake must have been
// built with RecordStdin.
func Stdin(t testing.TB, bin string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(bin), stdinFile))
	if err != nil {
		t.Fatalf("clitest: no recorded stdin (build the fake with RecordStdin and run it): %v", err)
	}
	return string(b)
}

// --- steps -----------------------------------------------------------------

type eventStep func(st *state) map[string]any

func (e eventStep) shell(b *strings.Builder, st *state) {
	m := e(st)
	if _, ok := m["session_id"]; !ok && st.session != "" {
		m["session_id"] = st.session
	}
	raw, _ := json.Marshal(m)
	fmt.Fprintf(b, "printf '%%s\\n' %s\n", quote(string(raw)))
}

func message(typ, role string, blocks ...map[string]any) eventStep {
	return func(*state) map[string]any {
		return map[string]any{"type": typ, "message": map[string]any{"role": role, "content": blocks}}
	}
}

// Init emits the system/init event and makes sessionID the session_id of every
// later event.
func Init(sessionID string) Step {
	return eventStep(func(st *state) map[string]any {
		st.session = sessionID
		return map[string]any{"type": "system", "subtype": "init", "session_id": sessionID}
	})
}

// Text emits an assistant message with one text block.
func Text(s string) Step {
	return message("assistant", "assistant", map[string]any{"type": "text", "text": s})
}

// ToolUse emits an assistant message calling tool name with the given JSON
// input (an empty inputJSON means {}). Invalid JSON is passed through as a
// string so a test can exercise malformed inputs without a panic.
func ToolUse(id, name, inputJSON string) Step {
	var in any = json.RawMessage(`{}`)
	if strings.TrimSpace(inputJSON) != "" {
		if json.Valid([]byte(inputJSON)) {
			in = json.RawMessage(inputJSON)
		} else {
			in = inputJSON
		}
	}
	return message("assistant", "assistant", map[string]any{"type": "tool_use", "id": id, "name": name, "input": in})
}

// ToolResult emits the user message carrying the result of tool call id.
func ToolResult(id, content string) Step {
	return message("user", "user", map[string]any{"type": "tool_result", "tool_use_id": id, "content": content})
}

// Result emits the final result event with the run's text and cost.
func Result(text string, costUSD float64) Step {
	return eventStep(func(*state) map[string]any {
		return map[string]any{"type": "result", "subtype": "success", "is_error": false,
			"result": text, "total_cost_usd": costUSD, "num_turns": 1}
	})
}

type rawStep string

// Raw emits line verbatim on stdout, for event types or garbage the builders
// do not cover (rate_limit_event, partial-message deltas, malformed JSON).
func Raw(line string) Step { return rawStep(line) }

func (r rawStep) shell(b *strings.Builder, _ *state) {
	fmt.Fprintf(b, "printf '%%s\\n' %s\n", quote(string(r)))
}

type stderrStep string

// Stderr writes s (plus a newline) to stderr.
func Stderr(s string) Step { return stderrStep(s) }

func (s stderrStep) shell(b *strings.Builder, _ *state) {
	fmt.Fprintf(b, "printf '%%s\\n' %s >&2\n", quote(string(s)))
}

type sleepStep time.Duration

// Sleep pauses the fake for d (rounded up to milliseconds), e.g. to exercise
// timeouts and cancellation.
func Sleep(d time.Duration) Step { return sleepStep(d) }

func (s sleepStep) shell(b *strings.Builder, _ *state) {
	ms := (time.Duration(s) + time.Millisecond - 1) / time.Millisecond
	// sleep accepts fractions on macOS and GNU coreutils; busybox needs ints
	// for older versions, so use whole seconds when the delay is whole.
	if ms%1000 == 0 {
		fmt.Fprintf(b, "sleep %d\n", ms/1000)
		return
	}
	fmt.Fprintf(b, "sleep %d.%03d\n", ms/1000, ms%1000)
}

type exitStep int

// ExitCode makes the script exit with n once all steps ran (the last
// ExitCode wins).
func ExitCode(n int) Step { return exitStep(n) }

func (exitStep) shell(*strings.Builder, *state) {}

type recordStdin struct{}

// RecordStdin makes the fake save its stdin for Stdin. It reads stdin to EOF
// at that point of the script, so put it first and only use it with callers
// that close stdin (runner does; an interactive client session does not).
func RecordStdin() Step { return recordStdin{} }

func (recordStdin) shell(b *strings.Builder, st *state) {
	fmt.Fprintf(b, "cat > %s\n", quote(filepath.Join(st.dir, stdinFile)))
}

type fileStep string

func (f fileStep) shell(b *strings.Builder, _ *state) {
	fmt.Fprintf(b, "cat %s\n", quote(string(f)))
}

type drainStdin struct{}

func (drainStdin) shell(b *strings.Builder, _ *state) { b.WriteString("cat >/dev/null\n") }
