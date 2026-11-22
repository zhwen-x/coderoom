// Package judge compiles and runs a submission against a problem's test data.
//
// M0 runs programs with a plain wall-clock timeout and no isolation. The
// Runner interface is the seam where M1 substitutes a Windows Job Object
// sandbox (memory / CPU / process-count / output limits) without touching the
// judging logic above it.
package judge

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"coderoom/internal/protocol"
)

// Toolchain holds the resolved absolute paths to the compilers and
// interpreters the judge uses. Empty means "not available".
type Toolchain struct {
	GXX    string `json:"gxx"`    // C++ compiler
	GCC    string `json:"gcc"`    // C compiler
	Python string `json:"python"` // Python interpreter
}

// Available reports which languages this toolchain can actually judge, so the
// teacher UI can warn before an exam rather than after.
func (t Toolchain) Available() map[string]bool {
	return map[string]bool{
		protocol.LangCPP:    t.GXX != "",
		protocol.LangC:      t.GCC != "",
		protocol.LangPython: t.Python != "",
	}
}

func (t Toolchain) pathFor(lang string) string {
	switch lang {
	case protocol.LangCPP:
		return t.GXX
	case protocol.LangC:
		return t.GCC
	case protocol.LangPython:
		return t.Python
	}
	return ""
}

// DetectToolchain resolves each tool in priority order:
//
//  1. an explicit override (teacher-configured path), so a teacher can pin the
//     judge to the same compiler version the lab's IDE uses;
//  2. the toolchain bundled next to the server executable, so the shipped
//     archive judges with zero configuration;
//  3. whatever is on PATH.
//
// Pinning matters: if the judge's gcc differs from the lab's, students hit
// "compiles locally, CE on the server".
func DetectToolchain(baseDir string, override Toolchain) Toolchain {
	var out Toolchain
	out.GXX = resolve(override.GXX, baseDir, bundledCandidates("g++"), pathCandidates("g++", "clang++", "c++"))
	out.GCC = resolve(override.GCC, baseDir, bundledCandidates("gcc"), pathCandidates("gcc", "clang", "cc"))
	out.Python = resolve(override.Python, baseDir, bundledPython(), pathCandidates("python3", "python"))
	return out
}

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

// bundledCandidates are the paths a shipped MinGW-w64 lands at, relative to
// the server executable's directory.
func bundledCandidates(tool string) []string {
	return []string{
		filepath.Join("toolchain", "mingw64", "bin", exeName(tool)),
		filepath.Join("toolchain", "bin", exeName(tool)),
	}
}

func bundledPython() []string {
	return []string{
		filepath.Join("toolchain", "python", exeName("python")),
		filepath.Join("toolchain", "python3", exeName("python3")),
	}
}

func pathCandidates(names ...string) []string { return names }

func resolve(override, baseDir string, bundled, onPath []string) string {
	if override != "" {
		if abs, err := filepath.Abs(override); err == nil {
			if st, err := os.Stat(abs); err == nil && !st.IsDir() {
				return abs
			}
		}
		// An override that does not exist is a configuration error worth
		// surfacing, not silently papering over - fall through to detection
		// but the teacher UI reports what was actually resolved.
	}
	for _, rel := range bundled {
		p := filepath.Join(baseDir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
		}
	}
	for _, name := range onPath {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// Options are the judge-wide compiler settings a teacher can adjust.
//
// The lab uses Visual Studio, Dev-C++, PyCharm and IDLE, so the judge only has
// to agree with whatever those produce for ordinary coursework code. The
// language standard is exposed because that is the one setting that can make
// code which builds in the lab fail on the server.
type Options struct {
	CPPStd string `json:"cpp_std"` // e.g. c++14
	CStd   string `json:"c_std"`   // e.g. c11
}

// Known standards. A typo here would make every submission in an exam fail to
// compile, so an unrecognised value falls back to the default rather than
// being passed to gcc.
var (
	knownCPPStd = map[string]bool{
		"c++98": true, "c++03": true, "c++11": true, "c++14": true,
		"c++17": true, "c++20": true, "gnu++11": true, "gnu++14": true,
		"gnu++17": true, "gnu++20": true,
	}
	knownCStd = map[string]bool{
		"c89": true, "c90": true, "c99": true, "c11": true, "c17": true,
		"gnu89": true, "gnu99": true, "gnu11": true, "gnu17": true,
	}
)

// DefaultOptions targets the common denominator of the lab's toolchains.
// C++14 is what current Visual Studio and any recent MinGW both accept.
func DefaultOptions() Options {
	return Options{CPPStd: "c++14", CStd: "c11"}
}

// Normalize replaces blank or unrecognised standards with the defaults and
// reports what it changed, so the teacher UI can say so instead of failing
// silently at exam time.
func (o Options) Normalize() (Options, []string) {
	def := DefaultOptions()
	var warnings []string

	if o.CPPStd == "" {
		o.CPPStd = def.CPPStd
	} else if !knownCPPStd[o.CPPStd] {
		warnings = append(warnings,
			fmt.Sprintf("无法识别的 C++ 标准 %q，已回退为 %s", o.CPPStd, def.CPPStd))
		o.CPPStd = def.CPPStd
	}
	if o.CStd == "" {
		o.CStd = def.CStd
	} else if !knownCStd[o.CStd] {
		warnings = append(warnings,
			fmt.Sprintf("无法识别的 C 标准 %q，已回退为 %s", o.CStd, def.CStd))
		o.CStd = def.CStd
	}
	return o, warnings
}

// Language describes how to build and run one language.
type Language struct {
	ID      string
	Name    string
	Ext     string
	RunArgs []string // placeholders {src} {exe}
}

// Languages is the supported language set.
var Languages = map[string]Language{
	protocol.LangCPP: {
		ID: protocol.LangCPP, Name: "C++", Ext: "cpp",
		RunArgs: []string{"{exe}"},
	},
	protocol.LangC: {
		ID: protocol.LangC, Name: "C", Ext: "c",
		RunArgs: []string{"{exe}"},
	},
	protocol.LangPython: {
		ID: protocol.LangPython, Name: "Python", Ext: "py",
		RunArgs: []string{"-u", "{src}"},
	},
}

// CompileArgs returns the arguments for the compile step, or nil when the
// language has none. Placeholders {src} and {exe} are substituted by the caller.
func (l Language) CompileArgs(opts Options) []string {
	switch l.ID {
	case protocol.LangCPP:
		return gccArgs(opts.CPPStd)
	case protocol.LangC:
		return gccArgs(opts.CStd)
	case protocol.LangPython:
		// py_compile turns a syntax error into a CE with a real message,
		// instead of a runtime error on every single test case.
		return []string{"-m", "py_compile", "{src}"}
	}
	return nil
}

func gccArgs(std string) []string {
	args := []string{"-O2", "-w"}
	if std != "" {
		args = append(args, "-std="+std)
	}
	if runtime.GOOS == "windows" {
		// Static linking avoids "libstdc++-6.dll not found" when the judge
		// runs a binary built by the bundled MinGW.
		args = append(args, "-static")
	}
	// Libraries go after the sources, which is what gcc's linker expects.
	return append(args, "-o", "{exe}", "{src}", "-lm")
}

// NeedsExecutable reports whether the compile step produces a binary that the
// run step then invokes (true for C/C++, false for Python).
func (l Language) NeedsExecutable() bool {
	for _, a := range l.RunArgs {
		if strings.Contains(a, "{exe}") {
			return true
		}
	}
	return false
}

func expand(args []string, repl map[string]string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		for k, v := range repl {
			a = strings.ReplaceAll(a, k, v)
		}
		out = append(out, a)
	}
	return out
}

// LanguageFor looks up a language, rejecting anything unknown so a tampered
// client cannot name an arbitrary command.
func LanguageFor(id string) (Language, error) {
	l, ok := Languages[id]
	if !ok {
		return Language{}, fmt.Errorf("unsupported language %q", id)
	}
	return l, nil
}
