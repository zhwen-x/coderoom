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
	// CPULimit is the problem's time limit. The default runner cannot enforce
	// CPU time directly, so it kills on WallLimit and then compares measured
	// CPU time against this.
	CPULimit      time.Duration
	WallLimit     time.Duration
	MemoryLimitMB int
	OutputLimit   int64 // bytes; 0 means unlimited
}

// RunResult is what the judge needs to assign a verdict.
type RunResult struct {
	ExitCode       int
	CPUMS          int // user + system CPU time
	WallMS         int
	MemoryKB       int  // peak RSS; 0 when the platform cannot report it
	TimedOut       bool // killed at WallLimit
	MemExceeded    bool // only set by a sandbox that enforces memory
	OutputExceeded bool
	Stderr         string
}

// Runner executes a process under whatever limits the platform supports.
//
// DefaultRunner (M0) enforces only a wall-clock kill and an output cap. M1
// adds a Windows Job Object implementation enforcing memory, CPU time and
// process count; the judge above this interface does not change.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// DefaultRunner runs the process with no isolation beyond a timeout.
//
// Known limits, stated plainly: a submission can allocate as much memory as
// the machine has, spawn child processes, and touch the filesystem. That is
// acceptable for M0 (link-up) and is what the M1 sandbox closes. It is NOT
// suitable for judging untrusted code from outside the classroom.
type DefaultRunner struct{}

func (DefaultRunner) Run(ctx context.Context, req RunRequest) (RunResult, error) {
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
	// Deny the child the parent's environment; a submission has no business
	// reading the server's env, and an empty env also keeps runs reproducible.
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

	start := time.Now()
	err := cmd.Run()
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
		res.MemoryKB = peakRSSKB(ps)
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
	}
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
	// Windows binaries need SystemRoot to load core DLLs; Python needs a
	// writable temp dir for its bytecode cache.
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
