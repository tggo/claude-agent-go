package runner

import (
	"errors"
	"fmt"
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
// exit code and a (token-redacted, truncated) snippet of stderr/stdout so
// callers can branch on the failure instead of string-matching.
type ProcessError struct {
	ExitCode int
	Stderr   string
	Err      error
}

func (e *ProcessError) Error() string {
	return fmt.Sprintf("claude cli exited with code %d: %s", e.ExitCode, e.Stderr)
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
