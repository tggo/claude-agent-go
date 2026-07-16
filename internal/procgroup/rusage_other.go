//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package procgroup

import "os"

// MaxRSSBytes reports no peak-memory figure: this platform's ProcessState does
// not carry a getrusage-style resident set size.
func MaxRSSBytes(_ *os.ProcessState) (int64, bool) { return 0, false }
