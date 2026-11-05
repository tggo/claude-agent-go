//go:build !windows

// Package procgroup centralizes process-group setup and teardown so the agent
// CLI and any children it spawns (git, make, tests) can be signalled together.
package procgroup

import (
	"os/exec"
	"syscall"
	"time"
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

// KillGraceful sends SIGTERM to the group, then SIGKILL if done has not closed
// within grace. A child that traps or ignores SIGTERM is therefore still torn
// down instead of leaving the caller blocked in Wait. It blocks until done is
// closed or the SIGKILL has been sent.
func KillGraceful(cmd *exec.Cmd, grace time.Duration, done <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	Kill(cmd)
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
