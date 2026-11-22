//go:build windows

package judge

import (
	"context"
	"strings"
	"testing"

	"coderoom/internal/protocol"
)

// These tests exercise the real Windows Job Object. They cannot run anywhere
// else, which is exactly why they exist: the sandbox is the one part of the
// judge that cross-compilation from macOS cannot verify.
//
// Two of them deliberately misbehave (exhausting memory, spawning processes).
// Both are bounded so that a *broken* sandbox fails the test rather than
// harming the machine running it.

// cppMemoryHog commits memory in 4 MiB chunks until the allocator refuses.
//
// The incremental shape matters. A single huge `new` would be rejected
// outright by the job's limit, leaving peak memory low and the verdict an
// ambiguous runtime error. Allocating gradually and touching each page is what
// drives measured peak memory past the problem's limit, which is what MLE is
// actually defined as here.
const cppMemoryHog = `
#include <cstdlib>
int main() {
    const size_t chunk = 4u << 20;          // 4 MiB
    for (int i = 0; i < 200; i++) {         // up to 800 MiB if unconstrained
        char* p = (char*)malloc(chunk);
        if (!p) return 1;                   // allocator refused: limit reached
        // One byte per 4 KiB page, each a different value. Touching every page
        // commits it; varying the value stops a host that compresses memory
        // from making the footprint disappear.
        for (size_t k = 0; k < chunk; k += 4096) p[k] = (char)(i + (int)(k >> 12));
    }
    return 0;
}`

func TestSandboxEnforcesMemoryLimit(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppMemoryHog,
		// 64 MiB limit; the job ceiling is that plus memoryHeadroom.
		Problem: Problem{TimeLimitMS: 3000, MemoryLimitMB: 64, ScoreMode: "sum"},
		Cases:   mustCases(t, [][2]string{{"", "ignored\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictMLE {
		t.Fatalf("verdict = %s, want MLE\n  峰值内存 %d KB (限制 %d KB)\n  编译日志: %s\n  测试点: %+v",
			res.Verdict, res.MemoryKB, 64*1024, res.CompileLog, res.Cases)
	}
	if res.Score != 0 {
		t.Errorf("score = %d, want 0 on MLE", res.Score)
	}
	// If the ceiling were not enforced the program would have reached 800 MiB.
	if res.MemoryKB > 400*1024 {
		t.Errorf("peak memory %d KB suggests the limit was not enforced", res.MemoryKB)
	}
}

// cppForkBomb tries to spawn children and counts only those that genuinely
// ran, identified by a sentinel exit code.
//
// Three details make this a meaningful assertion rather than a vacuous one:
//
//   - It re-launches its own executable rather than cmd.exe. A PATH lookup
//     that simply failed would also yield a count of zero, so the test would
//     pass whether or not the sandbox worked. argv[0] is an absolute path that
//     certainly exists.
//   - It counts children by exit code, not by CreateProcess returning success.
//     The job may let the call succeed and tear the child down immediately, in
//     which case a "created" count would be misleading.
//   - The loop is bounded and the child exits at once, so a *broken* sandbox
//     fails this test instead of flooding the machine.
//
// Unconstrained, this prints 5.
const cppForkBomb = `
#include <windows.h>
#include <cstdio>
int main(int argc, char** argv) {
    if (argc > 1) return 42;                // child branch: never spawns again
    int ran = 0;
    for (int i = 0; i < 5; i++) {
        STARTUPINFOA si; ZeroMemory(&si, sizeof(si)); si.cb = sizeof(si);
        PROCESS_INFORMATION pi; ZeroMemory(&pi, sizeof(pi));
        char cmd[1024];
        snprintf(cmd, sizeof(cmd), "\"%s\" child", argv[0]);
        if (CreateProcessA(NULL, cmd, NULL, NULL, FALSE,
                           CREATE_NO_WINDOW, NULL, NULL, &si, &pi)) {
            WaitForSingleObject(pi.hProcess, 500);
            DWORD code = 0;
            GetExitCodeProcess(pi.hProcess, &code);
            if (code == 42) ran++;          // the child really executed
            CloseHandle(pi.hProcess);
            CloseHandle(pi.hThread);
        }
    }
    printf("%d\n", ran);
    return 0;
}`

func TestSandboxBlocksChildProcesses(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	// Expecting "0" makes this a positive assertion: the program ran to
	// completion and reports that none of its children got to execute.
	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppForkBomb,
		Problem:  Problem{TimeLimitMS: 5000, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"", "0\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictAC {
		t.Fatalf("verdict = %s, want AC（程序应输出 0，表示没有子进程成功运行；"+
			"输出 5 则说明单进程限制未生效）\n  编译日志: %s\n  测试点: %+v",
			res.Verdict, res.CompileLog, res.Cases)
	}
}

// cppCrash dereferences a null pointer.
const cppCrash = `
int main() {
    volatile int* p = 0;
    *p = 1;
    return 0;
}`

func TestSandboxSuppressesCrashDialog(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	const limitMS = 1000
	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     cppCrash,
		Problem:  Problem{TimeLimitMS: limitMS, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"", "never reached\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	// Without JOB_OBJECT_LIMIT_DIE_ON_UNHANDLED_EXCEPTION, Windows Error
	// Reporting puts up a modal box and the run only ends when the wall-clock
	// backstop fires. TLE here means that suppression is not working.
	if res.Verdict == protocol.VerdictTLE {
		t.Fatalf("崩溃的提交被判为 TLE，说明崩溃对话框没有被抑制，判题被阻塞到墙钟超时")
	}
	if res.Verdict != protocol.VerdictRE {
		t.Fatalf("verdict = %s, want RE\n  编译日志: %s\n  测试点: %+v",
			res.Verdict, res.CompileLog, res.Cases)
	}
	// A crash should be reported in a fraction of the limit, not near it.
	if res.TimeMS > limitMS {
		t.Errorf("崩溃用了 %dms（限制 %dms），疑似被对话框拖住", res.TimeMS, limitMS)
	}
}

// TestSandboxAccountingIsSane guards the hand-declared
// jobBasicAccountingInformation layout and the PeakProcessMemoryUsed read.
// A field-offset mistake there would not fail to compile; it would silently
// report nonsense time and memory for every submission in the gradebook.
func TestSandboxAccountingIsSane(t *testing.T) {
	j := newTestJudger(t)
	requireLang(t, j, protocol.LangCPP)

	// Burns a measurable but small amount of CPU and memory.
	// The accumulator is volatile so -O2 cannot fold the loop into a closed
	// form; without that there would be no CPU time to measure. The expected
	// value is sum(i % 7) for i in [0, 20000000).
	const code = `
#include <cstdio>
#include <cstdlib>
#include <cstring>
int main() {
    const size_t n = 8u << 20;              // 8 MiB
    char* p = (char*)malloc(n);
    if (!p) return 1;
    memset(p, 1, n);
    volatile long long s = 0;
    for (int i = 0; i < 20000000; i++) s = s + (i % 7);
    printf("%lld\n", (long long)s);
    return 0;
}`
	res, err := j.Judge(context.Background(), Request{
		Language: protocol.LangCPP,
		Code:     code,
		Problem:  Problem{TimeLimitMS: 10000, MemoryLimitMB: 256, ScoreMode: "sum"},
		Cases:    mustCases(t, [][2]string{{"", "59999997\n"}}, 10),
	})
	if err != nil {
		t.Fatalf("judge error: %v", err)
	}
	if res.Verdict != protocol.VerdictAC {
		t.Fatalf("verdict = %s, want AC\n  编译日志: %s\n  测试点: %+v",
			res.Verdict, res.CompileLog, res.Cases)
	}

	// Memory: the program commits 8 MiB, so the figure must be well above zero
	// and nowhere near the 256 MiB limit.
	if res.MemoryKB < 4*1024 {
		t.Errorf("峰值内存 %d KB 过低，Job Object 内存计量可能读错了结构体字段", res.MemoryKB)
	}
	if res.MemoryKB > 200*1024 {
		t.Errorf("峰值内存 %d KB 过高，疑似读到了错误的字段", res.MemoryKB)
	}

	// CPU: a 20-million-iteration loop takes real time but not seconds.
	if res.TimeMS <= 0 {
		t.Errorf("CPU 时间为 %dms，Job Object 计时可能读错了结构体字段", res.TimeMS)
	}
	if res.TimeMS > 5000 {
		t.Errorf("CPU 时间 %dms 过高，疑似把 100ns 单位换算错了", res.TimeMS)
	}
}

// TestSandboxDescriptionMentionsEnforcement keeps the teacher-facing text in
// step with what this file proves is actually enforced.
func TestSandboxDescriptionMentionsEnforcement(t *testing.T) {
	got := SandboxDescription()
	for _, want := range []string{"Job Object", "内存", "CPU", "进程"} {
		if !strings.Contains(got, want) {
			t.Errorf("沙箱说明未提及 %q: %s", want, got)
		}
	}
}
