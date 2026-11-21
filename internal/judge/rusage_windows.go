//go:build windows

package judge

import "os"

// peakRSSKB has no portable source on Windows without a Job Object. M1
// replaces the whole runner with a Job Object implementation that reports
// real peak memory; until then memory is unmeasured and MLE is not detected.
func peakRSSKB(ps *os.ProcessState) int { return 0 }
