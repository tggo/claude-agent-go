//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package procgroup

import (
	"os"
	"syscall"
)

// MaxRSSBytes reports the peak resident set size of a finished process, and
// whether the platform could supply it. BSD-derived kernels (including macOS)
// report ru_maxrss in bytes, unlike Linux.
func MaxRSSBytes(st *os.ProcessState) (int64, bool) {
	if st == nil {
		return 0, false
	}
	ru, ok := st.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, false
	}
	return int64(ru.Maxrss), true
}
