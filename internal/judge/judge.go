package judge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"coderoom/internal/protocol"
)

// Problem is the judging-relevant subset of a problem. The judge package
// deliberately does not import the store, so it stays independently testable.
type Problem struct {
	TimeLimitMS   int
	MemoryLimitMB int
	ScoreMode     string // "sum" | "allornothing"
}

// TestCase points at one input/output file pair.
type TestCase struct {
	Index      int
	InputPath  string
	OutputPath string
	Score      int
}

// Request is one judging job.
type Request struct {
	Language string
	Code     string
	Problem  Problem
	Cases    []TestCase
}

// Result is the verdict for a whole submission. Score and MaxScore are in
// *test-case* scale (intrinsic to the problem). The gradebook scales that to
// the paper's per-problem mark, which keeps test data independent of how much
// any particular exam weights the problem.
type Result struct {
	Verdict    string
	Score      int
	MaxScore   int
	TimeMS     int
	MemoryKB   int
	CompileLog string
	Cases      []protocol.CaseResult
}

const (
	compileWallLimit = 15 * time.Second
	outputLimitBytes = 16 << 20 // 16 MiB of stdout is far past any real answer
)

// Judger compiles and runs submissions.
type Judger struct {
	tc       Toolchain
	opts     Options
	runner   Runner
	workRoot string
}

func New(tc Toolchain, runner Runner, workRoot string) *Judger {
	if runner == nil {
		runner = NewRunner()
	}
	return &Judger{tc: tc, opts: DefaultOptions(), runner: runner, workRoot: workRoot}
}

func (j *Judger) Toolchain() Toolchain { return j.tc }
func (j *Judger) Options() Options     { return j.opts }

// SetToolchain swaps the resolved compiler paths (teacher changed the config).
func (j *Judger) SetToolchain(tc Toolchain) { j.tc = tc }

// SetOptions swaps the compiler settings, falling back to the defaults for
// anything unrecognised and reporting what it rejected.
func (j *Judger) SetOptions(opts Options) []string {
	normalized, warnings := opts.Normalize()
	j.opts = normalized
	return warnings
}

func exeFileName() string {
	if isWindows() {
		return "prog.exe"
	}
	return "prog"
}

// Judge runs one submission to completion. It never returns an error for a
// student-caused outcome; those become verdicts. An error means the judge
// itself could not do its job.
func (j *Judger) Judge(ctx context.Context, req Request) (*Result, error) {
	lang, err := LanguageFor(req.Language)
	if err != nil {
		return systemError("不支持的语言: " + req.Language), nil
	}
	tool := j.tc.pathFor(req.Language)
	if tool == "" {
		return systemError(fmt.Sprintf(
			"服务端未找到 %s 的编译/运行环境，请在「判题环境」中配置路径", lang.Name)), nil
	}
	if len(req.Cases) == 0 {
		return systemError("该题目没有测试数据，无法判题"), nil
	}

	if err := os.MkdirAll(j.workRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create work root: %w", err)
	}
	dir, err := os.MkdirTemp(j.workRoot, "judge-")
	if err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	defer os.RemoveAll(dir)

	srcPath := filepath.Join(dir, "main."+lang.Ext)
	if err := os.WriteFile(srcPath, []byte(req.Code), 0o644); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	exePath := filepath.Join(dir, exeFileName())

	repl := map[string]string{
		"{src}": srcPath,
		"{exe}": exePath,
		"{dir}": dir,
	}

	// ---- compile -----------------------------------------------------------
	var compileLog string
	if compileArgs := lang.CompileArgs(j.opts); len(compileArgs) > 0 {
		argv := append([]string{tool}, expand(compileArgs, repl)...)
		cres, err := j.runner.Run(ctx, RunRequest{
			Dir:       dir,
			Argv:      argv,
			WallLimit: compileWallLimit,
			// Compilers need far more memory than a student program is
			// allowed, and gcc spawns cc1plus/as/ld, so the sandbox's memory
			// and process-count limits must not apply here.
			Compile: true,
		})
		if err != nil {
			return systemError("编译器启动失败: " + err.Error()), nil
		}
		compileLog = cres.Stderr
		if cres.TimedOut {
			return &Result{
				Verdict:    protocol.VerdictCE,
				MaxScore:   totalScore(req.Cases),
				CompileLog: "编译超时（超过 15 秒）\n" + compileLog,
				Cases:      []protocol.CaseResult{},
			}, nil
		}
		if cres.ExitCode != 0 {
			if compileLog == "" {
				compileLog = fmt.Sprintf("编译失败，退出码 %d", cres.ExitCode)
			}
			return &Result{
				Verdict:    protocol.VerdictCE,
				MaxScore:   totalScore(req.Cases),
				CompileLog: compileLog,
				Cases:      []protocol.CaseResult{},
			}, nil
		}
	}

	// ---- run every case ----------------------------------------------------
	runArgv := append([]string{tool}, expand(lang.RunArgs, repl)...)
	if lang.NeedsExecutable() {
		// For compiled languages the executable replaces the tool entirely.
		runArgv = expand(lang.RunArgs, repl)
	}

	cpuLimit := time.Duration(req.Problem.TimeLimitMS) * time.Millisecond
	if cpuLimit <= 0 {
		cpuLimit = time.Second
	}
	// Wall-clock backstop: generous enough not to punish slow I/O, tight
	// enough to stop a sleeping or blocked process from hanging the queue.
	wallLimit := cpuLimit*2 + 2*time.Second

	result := &Result{
		Verdict:    protocol.VerdictAC,
		MaxScore:   totalScore(req.Cases),
		CompileLog: compileLog,
		Cases:      make([]protocol.CaseResult, 0, len(req.Cases)),
	}
	allAC := true

	for _, tc := range req.Cases {
		outPath := filepath.Join(dir, fmt.Sprintf("out-%d.txt", tc.Index))
		rres, err := j.runner.Run(ctx, RunRequest{
			Dir:           dir,
			Argv:          runArgv,
			StdinPath:     tc.InputPath,
			StdoutPath:    outPath,
			CPULimit:      cpuLimit,
			WallLimit:     wallLimit,
			MemoryLimitMB: req.Problem.MemoryLimitMB,
			OutputLimit:   outputLimitBytes,
		})
		cr := protocol.CaseResult{Index: tc.Index, MaxScore: tc.Score}
		if err != nil {
			cr.Verdict = protocol.VerdictSE
			cr.Message = "运行失败: " + err.Error()
			allAC = false
			result.Cases = append(result.Cases, cr)
			continue
		}

		cr.TimeMS = reportedTimeMS(rres, cpuLimit)
		cr.MemoryKB = rres.MemoryKB
		if cr.TimeMS > result.TimeMS {
			result.TimeMS = cr.TimeMS
		}
		if cr.MemoryKB > result.MemoryKB {
			result.MemoryKB = cr.MemoryKB
		}

		switch {
		case rres.OutputExceeded:
			cr.Verdict = protocol.VerdictOLE
			cr.Message = "输出超过长度限制"
		case rres.TimedOut, rres.CPUMS > int(cpuLimit.Milliseconds()):
			cr.Verdict = protocol.VerdictTLE
			cr.Message = fmt.Sprintf("超过时间限制 %dms", cpuLimit.Milliseconds())
		case rres.MemExceeded:
			cr.Verdict = protocol.VerdictMLE
			cr.Message = fmt.Sprintf("超过内存限制 %dMB", req.Problem.MemoryLimitMB)
		case rres.ExitCode != 0:
			cr.Verdict = protocol.VerdictRE
			cr.Message = fmt.Sprintf("运行时错误，退出码 %d", rres.ExitCode)
			if rres.Stderr != "" {
				cr.Message += "\n" + truncate(rres.Stderr)
			}
		default:
			expected, rerr := os.ReadFile(tc.OutputPath)
			if rerr != nil {
				cr.Verdict = protocol.VerdictSE
				cr.Message = "读取标准答案失败: " + rerr.Error()
				break
			}
			actual, _ := os.ReadFile(outPath)
			if cmp := CompareOutput(expected, actual); cmp.OK {
				cr.Verdict = protocol.VerdictAC
				cr.Score = tc.Score
			} else {
				cr.Verdict = protocol.VerdictWA
				cr.Message = cmp.Message
			}
		}

		if cr.Verdict != protocol.VerdictAC {
			allAC = false
			if result.Verdict == protocol.VerdictAC {
				result.Verdict = cr.Verdict // first failing verdict wins overall
			}
		}
		result.Score += cr.Score
		result.Cases = append(result.Cases, cr)
	}

	if req.Problem.ScoreMode == "allornothing" {
		if allAC {
			result.Score = result.MaxScore
		} else {
			result.Score = 0
		}
	}
	return result, nil
}

// reportedTimeMS prefers measured CPU time but falls back to wall time on
// platforms that report no CPU accounting, so the teacher never sees 0ms for a
// program that clearly ran.
func reportedTimeMS(r RunResult, cpuLimit time.Duration) int {
	if r.CPUMS > 0 {
		return r.CPUMS
	}
	if r.TimedOut {
		return int(cpuLimit.Milliseconds())
	}
	return r.WallMS
}

func totalScore(cases []TestCase) int {
	n := 0
	for _, c := range cases {
		n += c.Score
	}
	return n
}

func systemError(msg string) *Result {
	return &Result{
		Verdict:    protocol.VerdictSE,
		CompileLog: msg,
		Cases:      []protocol.CaseResult{},
	}
}
