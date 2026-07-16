//go:build linux

package procgroup

import (
	"os"
	"syscall"
)

// MaxRSSBytes reports the peak resident set size of a finished process, and
// whether the platform could supply it. Linux's getrusage reports ru_maxrss in
// kilobytes.
func MaxRSSBytes(st *os.ProcessState) (int64, bool) {
	if st == nil {
		return 0, false
	}
	ru, ok := st.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0, false
	}
	return int64(ru.Maxrss) * 1024, true
}
