//go:build !windows

package judge

import (
	"os"
	"syscall"
)

// peakRSSKB reads max resident set size from wait4 accounting. Used on the
// macOS/Linux development machine; Windows gets its numbers from the Job
// Object in M1.
func peakRSSKB(ps *os.ProcessState) int {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	// Linux reports kilobytes, Darwin/BSD report bytes.
	if maxRSSInBytes {
		return int(ru.Maxrss / 1024)
	}
	return int(ru.Maxrss)
}
