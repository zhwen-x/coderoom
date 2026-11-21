package judge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"coderoom/internal/protocol"
)

// newTestJudger builds a judger against whatever compilers the development
// machine has. Languages that are unavailable are skipped rather than failed,
// so the suite still runs on a machine without, say, Python.
func newTestJudger(t *testing.T) *Judger {
	t.Helper()
	tc := DetectToolchain(t.TempDir(), Toolchain{})
	return New(tc, DefaultRunner{}, t.TempDir())
}

func mustCases(t *testing.T, pairs [][2]string, score int) []TestCase {
	t.Helper()
	dir := t.TempDir()
	cases := make([]TestCase, 0, len(pairs))
	for i, p := range pairs {
		in := filepath.Join(dir, fmt.Sprintf("%d.in", i+1))
		out := filepath.Join(dir, fmt.Sprintf("%d.out", i+1))
		if err := os.WriteFile(in, []byte(p[0]), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, []byte(p[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, TestCase{Index: i + 1, InputPath: in, OutputPath: out, Score: score})
	}
	return cases
}

func requireLang(t *testing.T, j *Judger, lang string) {
	t.Helper()
	if !j.Toolchain().Available()[lang] {
		t.Skipf("no toolchain for %s on this machine", lang)
	}
}

const cppSum = `#include <iostream>
int main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<std::endl;return 0;}`

func TestJudgeAccepted(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppSum,
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}, {"10 20\n", "30\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictAC {
		t.Fatalf("verdict = %s, want AC (log: %s, cases: %+v)",
			res.Verdict, res.CompileLog, res.Cases)
	}
	if res.Score != 20 || res.MaxScore != 20 {
		t.Errorf("score = %d/%d, want 20/20", res.Score, res.MaxScore)
	}
	if len(res.Cases) != 2 {
		t.Errorf("got %d case results, want 2", len(res.Cases))
	}
}

func TestJudgePartialScore(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	// Third case expects a deliberately wrong answer, so exactly 2 of 3 pass.
	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppSum,
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases: mustCases(t, [][2]string{
			{"1 2\n", "3\n"},
			{"4 5\n", "9\n"},
			{"1 1\n", "99\n"},
		}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictWA {
		t.Fatalf("verdict = %s, want WA", res.Verdict)
	}
	if res.Score != 20 || res.MaxScore != 30 {
		t.Errorf("score = %d/%d, want 20/30", res.Score, res.MaxScore)
	}
	if res.Cases[2].Message == "" {
		t.Error("failing case has no diagnostic message")
	}
}

func TestJudgeAllOrNothingScoring(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppSum,
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 256, ScoreMode: "allornothing"},
		Cases: mustCases(t, [][2]string{
			{"1 2\n", "3\n"},
			{"1 1\n", "99\n"},
		}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Score != 0 {
		t.Errorf("score = %d, want 0 under all-or-nothing with one failing case", res.Score)
	}
}

func TestJudgeCompileError(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     `int main(){ this is not c++ }`,
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 256},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictCE {
		t.Fatalf("verdict = %s, want CE", res.Verdict)
	}
	if res.CompileLog == "" {
		t.Error("CE with an empty compile log is useless to the student")
	}
	if res.Score != 0 {
		t.Errorf("score = %d, want 0 on CE", res.Score)
	}
}

func TestJudgeTimeLimitExceeded(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     `int main(){ volatile long long x=0; for(;;) x++; }`,
		Problem:  Problem{TimeLimitMS: 300, MemoryLimitMB: 256},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictTLE {
		t.Fatalf("verdict = %s, want TLE", res.Verdict)
	}
}

func TestJudgeRuntimeError(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     `int main(){ return 3; }`,
		Problem:  Problem{TimeLimitMS: 1000, MemoryLimitMB: 256},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictRE {
		t.Fatalf("verdict = %s, want RE", res.Verdict)
	}
}

func TestJudgePython(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangPython)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangPython,
		Code:     "a,b=map(int,input().split())\nprint(a+b)\n",
		Problem:  Problem{TimeLimitMS: 2000, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}, {"7 8\n", "15\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictAC {
		t.Fatalf("verdict = %s, want AC (log: %s, cases %+v)",
			res.Verdict, res.CompileLog, res.Cases)
	}
}

func TestJudgePythonSyntaxErrorIsCompileError(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangPython)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangPython,
		Code:     "def broken(:\n  pass\n",
		Problem:  Problem{TimeLimitMS: 2000, MemoryLimitMB: 256},
		Cases:    mustCases(t, [][2]string{{"1 2\n", "3\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictCE {
		t.Fatalf("verdict = %s, want CE for a Python syntax error", res.Verdict)
	}
}

func TestJudgeNoTestDataIsSystemError(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppSum,
		Problem:  Problem{TimeLimitMS: 1000},
		Cases:    nil,
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictSE {
		t.Fatalf("verdict = %s, want SE when a problem has no test data", res.Verdict)
	}
}

func TestJudgeUnknownLanguageRejected(t *testing.T) {
	j := newTestJudger(t)
	res, err := j.Judge(context.Background(), Request{
		Language: "../../bin/sh",
		Code:     "echo hi",
		Problem:  Problem{TimeLimitMS: 1000},
		Cases:    mustCases(t, [][2]string{{"", ""}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictSE {
		t.Fatalf("verdict = %s, want SE for an unknown language", res.Verdict)
	}
}
