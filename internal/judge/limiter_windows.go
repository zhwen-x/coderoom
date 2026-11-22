//go:build windows

package judge

import (
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobLimiter confines one execution to a Windows Job Object.
//
// What it enforces for a student program:
//
//   - peak memory (JOB_OBJECT_LIMIT_PROCESS_MEMORY / _JOB_MEMORY)
//   - CPU time   (JOB_OBJECT_LIMIT_PROCESS_TIME), with the wall clock as backstop
//   - one process only (JOB_OBJECT_LIMIT_ACTIVE_PROCESS), which stops fork bombs
//   - no crash dialog (JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION) - without
//     this a crashing submission pops a modal box and stalls the judge until
//     the wall-clock timeout
//   - no clipboard, desktop switching, display changes or ExitWindows
//   - everything in the job dies when the handle closes, so nothing survives
//     a judged run
//
// What it does NOT do: filesystem isolation. A submission can still read and
// write files the server account can reach. Closing that needs a restricted
// token or AppContainer and is deliberately left to a later milestone; in a
// classroom every submission is bound to a student number and is traceable.
type jobLimiter struct {
	req RunRequest
	job windows.Handle
}

func newLimiter(req RunRequest) limiter { return &jobLimiter{req: req} }

// memoryHeadroom is added to the problem's limit when setting the job's hard
// ceiling. The gap lets a program that merely overshoots run to completion so
// its real peak can be measured and reported as MLE, instead of being killed
// at exactly the limit and surfacing as an ambiguous runtime error.
const memoryHeadroom = 32 << 20 // 32 MiB

// jobBasicAccountingInformation mirrors JOBOBJECT_BASIC_ACCOUNTING_INFORMATION,
// which x/sys/windows does not declare. Times are in 100ns units.
type jobBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func (j *jobLimiter) prepare(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("创建 Job Object 失败: %w", err)
	}
	j.job = job

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags =
		windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE |
			windows.JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION

	// The compiler is deliberately exempt from the memory and process-count
	// limits: gcc legitimately uses hundreds of megabytes and spawns cc1plus,
	// as and ld. It still gets KILL_ON_JOB_CLOSE so no part of the toolchain
	// can outlive the judging of one submission.
	if !j.req.Compile {
		if j.req.MemoryLimitMB > 0 {
			ceiling := uintptr(j.req.MemoryLimitMB)<<20 + memoryHeadroom
			info.ProcessMemoryLimit = ceiling
			info.JobMemoryLimit = ceiling
			info.BasicLimitInformation.LimitFlags |=
				windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		}
		if j.req.CPULimit > 0 {
			info.BasicLimitInformation.PerProcessUserTimeLimit = int64(j.req.CPULimit.Nanoseconds() / 100)
			info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_PROCESS_TIME
		}
		info.BasicLimitInformation.ActiveProcessLimit = 1
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
	}

	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("设置 Job Object 资源限制失败: %w", err)
	}

	if !j.req.Compile {
		ui := windows.JOBOBJECT_BASIC_UI_RESTRICTIONS{
			UIRestrictionsClass: windows.JOB_OBJECT_UILIMIT_DESKTOP |
				windows.JOB_OBJECT_UILIMIT_DISPLAYSETTINGS |
				windows.JOB_OBJECT_UILIMIT_EXITWINDOWS |
				windows.JOB_OBJECT_UILIMIT_GLOBALATOMS |
				windows.JOB_OBJECT_UILIMIT_READCLIPBOARD |
				windows.JOB_OBJECT_UILIMIT_WRITECLIPBOARD |
				windows.JOB_OBJECT_UILIMIT_SYSTEMPARAMETERS,
		}
		if _, err := windows.SetInformationJobObject(job,
			windows.JobObjectBasicUIRestrictions,
			uintptr(unsafe.Pointer(&ui)), uint32(unsafe.Sizeof(ui))); err != nil {
			return fmt.Errorf("设置 Job Object 界面限制失败: %w", err)
		}
	}

	// CREATE_SUSPENDED is what removes the race: the process is created but
	// executes nothing until started() has put it inside the job.
	// CREATE_NO_WINDOW keeps a console from flashing for every test case.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	return nil
}

func (j *jobLimiter) started(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return fmt.Errorf("进程未启动")
	}
	pid := uint32(cmd.Process.Pid)

	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_INFORMATION,
		false, pid)
	if err != nil {
		return fmt.Errorf("打开进程 %d 失败: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.AssignProcessToJobObject(j.job, h); err != nil {
		return fmt.Errorf("将进程加入 Job Object 失败: %w", err)
	}
	// Only now is it safe to let the student's code run.
	return resumeMainThread(pid)
}

// resumeMainThread releases a process created with CREATE_SUSPENDED. Go's
// os/exec does not hand back the thread handle, so the thread is located by
// enumerating the system thread list; a suspended new process has exactly one.
func resumeMainThread(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("枚举线程失败: %w", err)
	}
	defer windows.CloseHandle(snap)

	var te windows.ThreadEntry32
	te.Size = uint32(unsafe.Sizeof(te))
	if err := windows.Thread32First(snap, &te); err != nil {
		return fmt.Errorf("枚举线程失败: %w", err)
	}
	for {
		if te.OwnerProcessID == pid {
			th, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
			if err != nil {
				return fmt.Errorf("打开主线程失败: %w", err)
			}
			_, err = windows.ResumeThread(th)
			windows.CloseHandle(th)
			if err != nil {
				return fmt.Errorf("恢复主线程失败: %w", err)
			}
			return nil
		}
		if err := windows.Thread32Next(snap, &te); err != nil {
			return fmt.Errorf("未找到进程 %d 的主线程: %w", pid, err)
		}
	}
}

func (j *jobLimiter) finish(cmd *exec.Cmd, res *RunResult) {
	if j.job == 0 {
		return
	}

	// Job accounting outlives the processes in it, so this is still valid
	// after the program has exited or been killed.
	var acct jobBasicAccountingInformation
	if err := windows.QueryInformationJobObject(j.job,
		windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&acct)), uint32(unsafe.Sizeof(acct)), nil); err == nil {
		res.CPUMS = int((acct.TotalUserTime + acct.TotalKernelTime) / 10_000) // 100ns -> ms
	}

	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(j.job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil); err == nil {
		res.MemoryKB = int(ext.PeakProcessMemoryUsed >> 10)
	}

	if !j.req.Compile && j.req.MemoryLimitMB > 0 && res.MemoryKB > j.req.MemoryLimitMB*1024 {
		res.MemExceeded = true
	}
}

// close terminates anything still alive in the job (KILL_ON_JOB_CLOSE) and
// releases the handle.
func (j *jobLimiter) close() {
	if j.job != 0 {
		windows.CloseHandle(j.job)
		j.job = 0
	}
}

// SandboxDescription states what is actually enforced on this platform, so the
// teacher's system panel shows the real protection level rather than implying
// one that does not exist.
func SandboxDescription() string {
	return "Windows Job Object：已启用内存上限、CPU 时间上限、单进程限制（防 fork 炸弹）、" +
		"崩溃对话框抑制、剪贴板与桌面限制。不含文件系统隔离。"
}
