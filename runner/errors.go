package runner

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/tggo/claude-agent-go/internal/procgroup"
)

// CLINotFoundError is returned when the claude binary (or the transport's launch
// command, e.g. docker/ssh) cannot be started — typically a missing executable.
type CLINotFoundError struct {
	Binary string
	Err    error
}

func (e *CLINotFoundError) Error() string {
	return fmt.Sprintf("claude binary %q could not be started: %v", e.Binary, e.Err)
}
func (e *CLINotFoundError) Unwrap() error { return e.Err }

// ProcessError is returned when the CLI runs but exits non-zero. It carries the
// exit code, (token-redacted, truncated) snippets of both output streams, and
// the OS's view of the finished process, so callers can branch on the failure
// instead of string-matching.
type ProcessError struct {
	ExitCode int

	// Stderr is what the CLI wrote to stderr. Empty when the process died
	// without writing any — see Stdout and ProcessState for what is left.
	Stderr string

	// Stdout is the tail of what the CLI wrote to stdout. Captured because a
	// process that dies mid-run often leaves stderr empty while stdout still
	// holds the last thing it did — a stream-json error event, or a JSON blob
	// truncated at the point of death. The tail, not the head, is kept: the end
	// is where the failure is.
	Stdout string

	// ProcessState is the OS's record of the finished process, or nil if it
	// never ran. It answers "how did it die" when neither stream did:
	// ProcessState.String() renders "signal: killed" for a SIGKILL — the shape
	// of an OOM kill from the parent's side — and SysUsage carries the
	// platform's resource accounting (see MaxRSSBytes).
	ProcessState *os.ProcessState

	Err error
}

// MaxRSSBytes reports the peak resident set size the process reached, and
// whether this platform supplies it (Linux, macOS, and the BSDs do). A peak
// sitting at the container's memory limit is the signature of an OOM kill —
// which, depending on the cgroup, can reach the parent as a plain non-zero exit
// with no output rather than as a signal.
func (e *ProcessError) MaxRSSBytes() (int64, bool) {
	return procgroup.MaxRSSBytes(e.ProcessState)
}

func (e *ProcessError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "claude cli exited with code %d", e.ExitCode)
	switch {
	case e.Stderr != "":
		fmt.Fprintf(&b, ": %s", e.Stderr)
	case e.Stdout != "":
		fmt.Fprintf(&b, " (no stderr; last stdout: %s)", e.Stdout)
	default:
		fmt.Fprintf(&b, " (no output on stdout or stderr%s)", e.postMortem())
	}
	return b.String()
}

// postMortem renders whatever the OS can still tell us about a process that
// died in silence, so the error is never just an exit code with nothing after
// the colon.
func (e *ProcessError) postMortem() string {
	if e.ProcessState == nil {
		return ""
	}
	var b strings.Builder
	if !e.ProcessState.Exited() {
		// Killed by a signal: ExitCode() is -1, so the state string carries the
		// only real information here.
		fmt.Fprintf(&b, "; %s", e.ProcessState)
	}
	if rss, ok := e.MaxRSSBytes(); ok {
		fmt.Fprintf(&b, "; peak rss %.1f MiB", float64(rss)/(1<<20))
	}
	return b.String()
}

func (e *ProcessError) Unwrap() error { return e.Err }

// TimeoutError is returned when the invocation exceeds ProcessTimeout.
type TimeoutError struct {
	Timeout string
	Err     error
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("claude cli timed out after %s", e.Timeout)
}
func (e *TimeoutError) Unwrap() error { return e.Err }

// IsCLINotFound reports whether err is (or wraps) a CLINotFoundError.
func IsCLINotFound(err error) bool {
	var e *CLINotFoundError
	return errors.As(err, &e)
}

// IsProcessError reports whether err is (or wraps) a ProcessError, and returns
// it for inspection (exit code, stderr).
func IsProcessError(err error) (*ProcessError, bool) {
	var e *ProcessError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// IsTimeout reports whether err is (or wraps) a TimeoutError.
func IsTimeout(err error) bool {
	var e *TimeoutError
	return errors.As(err, &e)
}

// exitCodeOf extracts the process exit code from a wait error, or -1.
func exitCodeOf(err error) int {
	var ee interface{ ExitCode() int }
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
