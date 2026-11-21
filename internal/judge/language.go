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

// Language describes how to build and run one language.
type Language struct {
	ID   string
	Name string
	Ext  string
	// CompileArgs/RunArgs use the placeholders {tool} {src} {exe} {dir}.
	CompileArgs []string
	RunArgs     []string
}

// Languages is the M0/M1 language set.
var Languages = map[string]Language{
	protocol.LangCPP: {
		ID: protocol.LangCPP, Name: "C++", Ext: "cpp",
		CompileArgs: cppCompileArgs(),
		RunArgs:     []string{"{exe}"},
	},
	protocol.LangC: {
		ID: protocol.LangC, Name: "C", Ext: "c",
		CompileArgs: cCompileArgs(),
		RunArgs:     []string{"{exe}"},
	},
	protocol.LangPython: {
		ID: protocol.LangPython, Name: "Python", Ext: "py",
		// py_compile turns a syntax error into a CE with a real message,
		// instead of a runtime error on every single test case.
		CompileArgs: []string{"-m", "py_compile", "{src}"},
		RunArgs:     []string{"-u", "{src}"},
	},
}

func cppCompileArgs() []string {
	args := []string{"-O2", "-std=c++14", "-w", "-o", "{exe}", "{src}"}
	if runtime.GOOS == "windows" {
		// Static linking avoids "libstdc++-6.dll not found" when the judge
		// runs a binary built by the bundled MinGW.
		args = append([]string{"-static"}, args...)
	}
	return append(args, "-lm")
}

func cCompileArgs() []string {
	args := []string{"-O2", "-std=c11", "-w", "-o", "{exe}", "{src}"}
	if runtime.GOOS == "windows" {
		args = append([]string{"-static"}, args...)
	}
	return append(args, "-lm")
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
