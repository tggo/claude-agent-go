//go:build !windows

// Package procgroup centralizes process-group setup and teardown so the agent
// CLI and any children it spawns (git, make, tests) can be signalled together.
package procgroup

import (
	"os/exec"
	"syscall"
)

// Setup puts the child in its own process group.
func Setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Kill sends SIGTERM to the entire group (negative PID). Errors are ignored —
// the process may already have exited.
func Kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}
