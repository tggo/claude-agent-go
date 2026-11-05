//go:build windows

package procgroup

import (
	"os/exec"
	"time"
)

// Setup is a no-op on Windows.
func Setup(_ *exec.Cmd) {}

// Kill is a no-op on Windows.
func Kill(_ *exec.Cmd) {}

// KillGraceful kills the process outright; Windows has no SIGTERM equivalent.
func KillGraceful(cmd *exec.Cmd, _ time.Duration, _ <-chan struct{}) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
