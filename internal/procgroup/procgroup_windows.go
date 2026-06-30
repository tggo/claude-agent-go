//go:build windows

package procgroup

import "os/exec"

// Setup is a no-op on Windows.
func Setup(_ *exec.Cmd) {}

// Kill is a no-op on Windows.
func Kill(_ *exec.Cmd) {}
