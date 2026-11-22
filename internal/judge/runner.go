package judge

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

// RunRequest is one process execution.
type RunRequest struct {
	Dir        string   // working directory
	Argv       []string // argv[0] must be an absolute path
	StdinPath  string   // "" means no stdin
	StdoutPath string   // "" means discard

	// CPULimit is the problem's time limit. Platforms that can enforce CPU
	// time do so; the wall-clock kill below is the backstop everywhere.
	CPULimit      time.Duration
	WallLimit     time.Duration
	MemoryLimitMB int
	OutputLimit   int64 // bytes; 0 means unlimited

	// Compile is set for the compiler invocation rather than the student's
	// program. Compilers legitimately use lots of memory and spawn child
	// processes (cc1plus, as, ld), so the sandbox's memory and process-count
	// limits must not apply to them.
	Compile bool
}

// RunResult is what the judge needs to assign a verdict.
type RunResult struct {
	ExitCode       int
	CPUMS          int // user + kernel CPU time
	WallMS         int
	MemoryKB       int  // peak memory; 0 when the platform cannot report it
	TimedOut       bool // killed at WallLimit
	MemExceeded    bool // peak memory went past the problem's limit
	OutputExceeded bool
	Stderr         string
}

// Runner executes a process under whatever limits the platform supports.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// NewRunner returns the best runner for this platform.
//
// On Windows that is a Job Object sandbox enforcing memory, CPU time and
// process count. Elsewhere (the development machine) only the wall-clock kill
// and the output cap apply — enough to develop and test the judging logic,
// but not a sandbox. The production target is Windows.
func NewRunner() Runner { return execRunner{} }

// execRunner holds the process plumbing shared by every platform: redirection,
// output capping, timeout and exit handling. Everything platform-specific
// lives behind the limiter interface, which keeps the untestable surface small.
type execRunner struct{}

// limiter is the platform's resource-enforcement hook around one execution.
//
// The call order is prepare -> (process created) -> started -> (process runs)
// -> finish -> close. On Windows the process is created suspended so that
// `started` can attach it to the job before a single instruction of the
// student's code runs; there is no window in which it is unconstrained.
type limiter interface {
	prepare(cmd *exec.Cmd) error
	started(cmd *exec.Cmd) error
	finish(cmd *exec.Cmd, res *RunResult)
	close()
}

func (execRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	if len(req.Argv) == 0 {
		return RunResult{}, errors.New("empty argv")
	}
	wall := req.WallLimit
	if wall <= 0 {
		wall = 10 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, wall)
	defer cancel()

	cmd := exec.CommandContext(runCtx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	// Deny the child the parent's environment: a submission has no business
	// reading the server's env, and a fixed env also keeps runs reproducible.
	cmd.Env = minimalEnv()

	if req.StdinPath != "" {
		f, err := os.Open(req.StdinPath)
		if err != nil {
			return RunResult{}, err
		}
		defer f.Close()
		cmd.Stdin = f
	}

	var outCap *cappedFile
	if req.StdoutPath != "" {
		f, err := os.Create(req.StdoutPath)
		if err != nil {
			return RunResult{}, err
		}
		defer f.Close()
		outCap = &cappedFile{f: f, limit: req.OutputLimit}
		cmd.Stdout = outCap
	}

	errCap := &cappedBuffer{limit: 16 << 10}
	cmd.Stderr = errCap

	// Bound two sources of unexpected delay in Wait: a process that ignores
	// the context cancellation, and one that exits but leaves a grandchild
	// holding the stderr pipe (gcc spawning cc1plus does exactly this).
	cmd.WaitDelay = 3 * time.Second

	lim := newLimiter(req)
	defer lim.close()
	if err := lim.prepare(cmd); err != nil {
		return RunResult{}, err
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}
	if err := lim.started(cmd); err != nil {
		// The process exists but could not be constrained. Kill it rather
		// than letting unconstrained student code run.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return RunResult{}, err
	}

	err := cmd.Wait()
	wallMS := int(time.Since(start).Milliseconds())

	res := RunResult{
		WallMS: wallMS,
		Stderr: trimForLog(errCap.Bytes(), 8<<10),
	}
	if outCap != nil {
		res.OutputExceeded = outCap.exceeded
	}
	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
		res.CPUMS = int((ps.UserTime() + ps.SystemTime()).Milliseconds())
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
	}

	// Platform accounting overrides the portable numbers where it is better.
	lim.finish(cmd, &res)

	// A failure to start (binary missing, not executable) is a judge-side
	// problem, not a student verdict; surface it as an error.
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) && !res.TimedOut {
		return res, err
	}
	return res, nil
}

func minimalEnv() []string {
	env := []string{}
	// Windows binaries need SystemRoot to load core DLLs; compilers and Python
	// need PATH and a writable temp dir.
	for _, k := range []string{"SystemRoot", "windir", "TEMP", "TMP", "TMPDIR", "PATH"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// cappedFile stops writing once limit bytes have been written, so a program
// printing in an infinite loop cannot fill the disk.
type cappedFile struct {
	f        *os.File
	limit    int64
	written  int64
	exceeded bool
}

func (c *cappedFile) Write(p []byte) (int, error) {
	if c.limit > 0 && c.written >= c.limit {
		c.exceeded = true
		return len(p), nil // absorb silently; the verdict is OLE
	}
	if c.limit > 0 && c.written+int64(len(p)) > c.limit {
		allow := c.limit - c.written
		if _, err := c.f.Write(p[:allow]); err != nil {
			return 0, err
		}
		c.written = c.limit
		c.exceeded = true
		return len(p), nil
	}
	n, err := c.f.Write(p)
	c.written += int64(n)
	return n, err
}

type cappedBuffer struct {
	buf   []byte
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := c.limit - len(c.buf); room > 0 {
		if len(p) <= room {
			c.buf = append(c.buf, p...)
		} else {
			c.buf = append(c.buf, p[:room]...)
		}
	}
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte { return c.buf }
