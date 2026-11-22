package judge

import (
	"strings"
	"testing"

	"coderoom/internal/protocol"
)

func TestOptionsNormalize(t *testing.T) {
	def := DefaultOptions()
	tests := []struct {
		name        string
		in          Options
		wantCPP     string
		wantC       string
		wantWarning bool
	}{
		{"空值取默认", Options{}, def.CPPStd, def.CStd, false},
		{"合法标准保留", Options{CPPStd: "c++17", CStd: "c99"}, "c++17", "c99", false},
		{"GNU 方言也接受", Options{CPPStd: "gnu++17", CStd: "gnu11"}, "gnu++17", "gnu11", false},
		// A typo here would make every submission in an exam fail to compile,
		// so it must degrade to the default rather than reach gcc.
		{"C++ 标准拼错", Options{CPPStd: "c++99", CStd: "c11"}, def.CPPStd, "c11", true},
		{"C 标准拼错", Options{CPPStd: "c++14", CStd: "c23x"}, "c++14", def.CStd, true},
		{"注入尝试", Options{CPPStd: "c++14 -lfoo"}, def.CPPStd, def.CStd, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := tc.in.Normalize()
			if got.CPPStd != tc.wantCPP || got.CStd != tc.wantC {
				t.Errorf("got %+v, want cpp=%s c=%s", got, tc.wantCPP, tc.wantC)
			}
			if (len(warnings) > 0) != tc.wantWarning {
				t.Errorf("warnings = %v, wantWarning = %v", warnings, tc.wantWarning)
			}
		})
	}
}

func TestCompileArgsUseConfiguredStandard(t *testing.T) {
	cpp := Languages[protocol.LangCPP]
	args := strings.Join(cpp.CompileArgs(Options{CPPStd: "c++17"}), " ")
	if !strings.Contains(args, "-std=c++17") {
		t.Errorf("C++ compile args missing the configured standard: %s", args)
	}

	c := Languages[protocol.LangC]
	args = strings.Join(c.CompileArgs(Options{CStd: "c99"}), " ")
	if !strings.Contains(args, "-std=c99") {
		t.Errorf("C compile args missing the configured standard: %s", args)
	}

	// Libraries must come after the sources or gcc's linker drops them.
	all := c.CompileArgs(Options{CStd: "c11"})
	srcAt, libAt := -1, -1
	for i, a := range all {
		if a == "{src}" {
			srcAt = i
		}
		if a == "-lm" {
			libAt = i
		}
	}
	if srcAt < 0 || libAt < 0 || libAt < srcAt {
		t.Errorf("link order wrong: %v", all)
	}

	// An empty standard must not produce a bare "-std=".
	for _, a := range cpp.CompileArgs(Options{}) {
		if a == "-std=" {
			t.Error("empty standard produced an invalid -std= flag")
		}
	}

	if py := Languages[protocol.LangPython].CompileArgs(Options{}); len(py) == 0 {
		t.Error("Python should still have a syntax-check step")
	}
}

func TestSetOptionsReportsRejectedValues(t *testing.T) {
	j := New(Toolchain{GXX: "/fake/g++"}, &fakeRunner{}, t.TempDir())
	if warnings := j.SetOptions(Options{CPPStd: "c++17", CStd: "c11"}); len(warnings) != 0 {
		t.Errorf("valid options produced warnings: %v", warnings)
	}
	if got := j.Options().CPPStd; got != "c++17" {
		t.Errorf("option not applied: %s", got)
	}
	warnings := j.SetOptions(Options{CPPStd: "nonsense"})
	if len(warnings) == 0 {
		t.Error("a bad standard was accepted silently")
	}
	if j.Options().CPPStd != DefaultOptions().CPPStd {
		t.Errorf("bad standard not replaced by the default: %s", j.Options().CPPStd)
	}
}

func TestSandboxDescriptionIsHonest(t *testing.T) {
	// The teacher panel shows this verbatim; it must not imply protection the
	// platform does not provide.
	got := SandboxDescription()
	if got == "" {
		t.Fatal("empty sandbox description")
	}
	if isWindows() {
		if !strings.Contains(got, "Job Object") {
			t.Errorf("Windows description should name the mechanism: %s", got)
		}
	} else if !strings.Contains(got, "无沙箱") {
		t.Errorf("non-Windows description must say there is no sandbox: %s", got)
	}
}
