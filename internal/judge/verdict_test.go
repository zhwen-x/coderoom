package judge

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"coderoom/internal/protocol"
)

// fakeRunner stands in for the platform sandbox so the verdict logic can be
// exercised on any host. MLE in particular is only ever produced by the
// Windows Job Object, which cannot run on the development machine.
type fakeRunner struct {
	compile    RunResult
	compileErr error

	runs    []RunResult // one per test case, in order
	stdouts []string    // what the program "prints" for each case
	runErr  error

	compileReqs []RunRequest
	runReqs     []RunRequest
}

func (f *fakeRunner) Run(_ context.Context, req RunRequest) (RunResult, error) {
	if req.Compile {
		f.compileReqs = append(f.compileReqs, req)
		return f.compile, f.compileErr
	}
	i := len(f.runReqs)
	f.runReqs = append(f.runReqs, req)
	if f.runErr != nil {
		return RunResult{}, f.runErr
	}
	if req.StdoutPath != "" {
		out := ""
		if i < len(f.stdouts) {
			out = f.stdouts[i]
		}
		if err := os.WriteFile(req.StdoutPath, []byte(out), 0o644); err != nil {
			panic(err)
		}
	}
	if i < len(f.runs) {
		return f.runs[i], nil
	}
	return RunResult{}, nil
}

// fakeJudger wires a judger to a fake toolchain, so no real compiler is needed.
func fakeJudger(t *testing.T, r *fakeRunner) *Judger {
	t.Helper()
	return New(Toolchain{GXX: "/fake/g++", GCC: "/fake/gcc", Python: "/fake/python"},
		r, t.TempDir())
}

func req3Cases(t *testing.T) Request {
	t.Helper()
	return Request{
		Language: protocol.LangCPP,
		Code:     "int main(){}",
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 64, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"a", "1\n"}, {"b", "2\n"}, {"c", "3\n"}}, 10),
	}
}

func TestVerdictMemoryLimitExceeded(t *testing.T) {
	r := &fakeRunner{
		runs:    []RunResult{{MemExceeded: true, MemoryKB: 70 * 1024, CPUMS: 20}},
		stdouts: []string{"1\n"},
	}
	j := fakeJudger(t, r)

	req := req3Cases(t)
	req.Cases = req.Cases[:1]
	res, err := j.Judge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictMLE {
		t.Fatalf("verdict = %s, want MLE", res.Verdict)
	}
	// MLE must beat the output comparison: the output happens to be correct
	// here, and it still must not score.
	if res.Score != 0 {
		t.Errorf("score = %d, want 0 on MLE", res.Score)
	}
	if res.MemoryKB != 70*1024 {
		t.Errorf("peak memory not reported: %d", res.MemoryKB)
	}
}

func TestVerdictOutputLimitExceeded(t *testing.T) {
	r := &fakeRunner{
		runs:    []RunResult{{OutputExceeded: true}},
		stdouts: []string{"1\n"},
	}
	j := fakeJudger(t, r)
	req := req3Cases(t)
	req.Cases = req.Cases[:1]

	res, err := j.Judge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictOLE {
		t.Fatalf("verdict = %s, want OLE", res.Verdict)
	}
}

func TestVerdictFirstFailureWinsOverall(t *testing.T) {
	// case 1 passes, case 2 times out, case 3 is wrong.
	r := &fakeRunner{
		runs: []RunResult{
			{CPUMS: 10},
			{TimedOut: true},
			{CPUMS: 15},
		},
		stdouts: []string{"1\n", "", "WRONG\n"},
	}
	j := fakeJudger(t, r)

	res, err := j.Judge(context.Background(), req3Cases(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictTLE {
		t.Fatalf("overall verdict = %s, want the first failure (TLE)", res.Verdict)
	}
	if res.Score != 10 || res.MaxScore != 30 {
		t.Errorf("score = %d/%d, want 10/30", res.Score, res.MaxScore)
	}
	want := []string{protocol.VerdictAC, protocol.VerdictTLE, protocol.VerdictWA}
	for i, w := range want {
		if res.Cases[i].Verdict != w {
			t.Errorf("case %d verdict = %s, want %s", i+1, res.Cases[i].Verdict, w)
		}
	}
	// Every case runs even after a failure, so partial credit is accurate.
	if len(r.runReqs) != 3 {
		t.Errorf("ran %d cases, want all 3", len(r.runReqs))
	}
}

func TestVerdictRuntimeErrorCarriesStderr(t *testing.T) {
	r := &fakeRunner{
		runs:    []RunResult{{ExitCode: 3, Stderr: "Segmentation fault"}},
		stdouts: []string{""},
	}
	j := fakeJudger(t, r)
	req := req3Cases(t)
	req.Cases = req.Cases[:1]

	res, err := j.Judge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictRE {
		t.Fatalf("verdict = %s, want RE", res.Verdict)
	}
	if msg := res.Cases[0].Message; msg == "" {
		t.Error("RE with no diagnostic is useless to the student")
	}
}

func TestVerdictRunnerFailureIsSystemError(t *testing.T) {
	// A judge-side failure must not be reported to the student as a wrong
	// answer; it is the operator's problem, not theirs.
	r := &fakeRunner{runErr: errors.New("沙箱初始化失败")}
	j := fakeJudger(t, r)
	req := req3Cases(t)
	req.Cases = req.Cases[:1]

	res, err := j.Judge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Cases[0].Verdict != protocol.VerdictSE {
		t.Fatalf("case verdict = %s, want SE", res.Cases[0].Verdict)
	}
	if res.Score != 0 {
		t.Errorf("score = %d, want 0", res.Score)
	}
}

func TestCompileIsExemptFromSandboxLimits(t *testing.T) {
	r := &fakeRunner{runs: []RunResult{{}, {}, {}}, stdouts: []string{"1\n", "2\n", "3\n"}}
	j := fakeJudger(t, r)

	if _, err := j.Judge(context.Background(), req3Cases(t)); err != nil {
		t.Fatal(err)
	}

	if len(r.compileReqs) != 1 {
		t.Fatalf("compiled %d times, want 1", len(r.compileReqs))
	}
	// The compiler legitimately needs lots of memory and spawns child
	// processes; applying the student program's limits to it would make every
	// submission fail to build.
	if !r.compileReqs[0].Compile {
		t.Error("compile request not flagged as a compile")
	}
	if r.compileReqs[0].MemoryLimitMB != 0 {
		t.Errorf("compile carries a memory limit of %d MB", r.compileReqs[0].MemoryLimitMB)
	}

	for i, rr := range r.runReqs {
		if rr.Compile {
			t.Errorf("run %d flagged as a compile", i)
		}
		if rr.MemoryLimitMB != 64 {
			t.Errorf("run %d memory limit = %d, want 64", i, rr.MemoryLimitMB)
		}
		if rr.CPULimit != time.Second {
			t.Errorf("run %d CPU limit = %v, want 1s", i, rr.CPULimit)
		}
		// The wall clock is the backstop for a process that blocks rather
		// than burning CPU, so it must be strictly longer.
		if rr.WallLimit <= rr.CPULimit {
			t.Errorf("run %d wall limit %v not greater than CPU limit %v",
				i, rr.WallLimit, rr.CPULimit)
		}
		if rr.OutputLimit <= 0 {
			t.Errorf("run %d has no output cap; a printing loop could fill the disk", i)
		}
		if rr.StdinPath == "" || rr.StdoutPath == "" {
			t.Errorf("run %d missing stdin/stdout redirection", i)
		}
	}
}

func TestCompileFailureSkipsExecution(t *testing.T) {
	r := &fakeRunner{compile: RunResult{ExitCode: 1, Stderr: "error: expected ';'"}}
	j := fakeJudger(t, r)

	res, err := j.Judge(context.Background(), req3Cases(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictCE {
		t.Fatalf("verdict = %s, want CE", res.Verdict)
	}
	if len(r.runReqs) != 0 {
		t.Errorf("ran %d cases after a compile error, want 0", len(r.runReqs))
	}
	if res.MaxScore != 30 {
		t.Errorf("max score = %d, want 30 even on CE", res.MaxScore)
	}
}

func TestCPUTimeBeyondLimitIsTLEWithoutWallClockKill(t *testing.T) {
	// The Job Object kills on CPU time, so the process exits normally while
	// having burned more CPU than allowed. That must still be TLE.
	r := &fakeRunner{
		runs:    []RunResult{{CPUMS: 1500, ExitCode: 0}},
		stdouts: []string{"1\n"},
	}
	j := fakeJudger(t, r)
	req := req3Cases(t)
	req.Cases = req.Cases[:1]

	res, err := j.Judge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Verdict != protocol.VerdictTLE {
		t.Fatalf("verdict = %s, want TLE (CPU %dms over a 1000ms limit)",
			res.Verdict, 1500)
	}
}

func TestReportedTimeFallsBackToWallClock(t *testing.T) {
	// Some platforms report no CPU accounting; the teacher should not see 0ms
	// for a program that obviously ran.
	if got := reportedTimeMS(RunResult{CPUMS: 0, WallMS: 42}, time.Second); got != 42 {
		t.Errorf("reportedTimeMS fell back to %d, want 42", got)
	}
	if got := reportedTimeMS(RunResult{CPUMS: 17, WallMS: 900}, time.Second); got != 17 {
		t.Errorf("reportedTimeMS = %d, want the CPU figure 17", got)
	}
	if got := reportedTimeMS(RunResult{TimedOut: true}, 300*time.Millisecond); got != 300 {
		t.Errorf("timed-out run reported %dms, want the limit 300", got)
	}
}
