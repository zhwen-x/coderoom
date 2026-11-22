//go:build !windows

package judge

import (
	"os/exec"
)

// noopLimiter is the development-machine path. It enforces nothing beyond the
// wall-clock kill and output cap that execRunner applies everywhere; it only
// measures, so the judging logic above can be exercised locally.
//
// Stated plainly: on this path a submission can allocate all of the machine's
// memory, spawn child processes and touch the filesystem. The production
// target is Windows, where limiter_windows.go closes all three.
type noopLimiter struct {
	memoryLimitMB int
	isCompile     bool
}

func newLimiter(req RunRequest) limiter {
	return &noopLimiter{memoryLimitMB: req.MemoryLimitMB, isCompile: req.Compile}
}

func (*noopLimiter) prepare(*exec.Cmd) error { return nil }
func (*noopLimiter) started(*exec.Cmd) error { return nil }
func (*noopLimiter) close()                  {}

func (l *noopLimiter) finish(cmd *exec.Cmd, res *RunResult) {
	if cmd.ProcessState == nil {
		return
	}
	res.MemoryKB = peakRSSKB(cmd.ProcessState)

	// Memory is measured but not enforced here, so MLE is still reported when
	// the measurement exceeds the problem's limit. The compiler is exempt:
	// gcc routinely uses far more memory than a student program is allowed.
	if !l.isCompile && l.memoryLimitMB > 0 && res.MemoryKB > l.memoryLimitMB*1024 {
		res.MemExceeded = true
	}
}

// SandboxDescription states what is actually enforced on this platform.
func SandboxDescription() string {
	return "无沙箱（非 Windows 开发环境）：仅有墙钟超时与输出长度上限，" +
		"不限制内存、进程数或文件访问。正式部署环境为 Windows。"
}
