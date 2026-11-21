package judge

import (
	"bytes"
	"fmt"
	"strings"
)

// CompareResult is the outcome of checking one test case's output.
type CompareResult struct {
	OK      bool
	Message string // first difference, for the teacher's eyes
}

// CompareOutput applies the default comparison rules:
//
//   - CRLF and CR are normalised to LF, so a Windows-built program that emits
//     "\r\n" is not marked wrong;
//   - trailing whitespace on each line is ignored;
//   - trailing blank lines at the end of the file are ignored.
//
// Everything else is a strict match. Floating-point tolerance and special
// judges are deliberately out of scope until M5.
func CompareOutput(expected, actual []byte) CompareResult {
	exp := normalizeLines(expected)
	act := normalizeLines(actual)

	if len(exp) == len(act) {
		same := true
		for i := range exp {
			if exp[i] != act[i] {
				same = false
				break
			}
		}
		if same {
			return CompareResult{OK: true}
		}
	}

	n := len(exp)
	if len(act) > n {
		n = len(act)
	}
	for i := 0; i < n; i++ {
		var e, a string
		var eOK, aOK bool
		if i < len(exp) {
			e, eOK = exp[i], true
		}
		if i < len(act) {
			a, aOK = act[i], true
		}
		if e == a && eOK == aOK {
			continue
		}
		switch {
		case !aOK:
			return CompareResult{Message: fmt.Sprintf(
				"第 %d 行: 选手输出提前结束，期望 %q", i+1, truncate(e))}
		case !eOK:
			return CompareResult{Message: fmt.Sprintf(
				"第 %d 行: 选手输出多余内容 %q", i+1, truncate(a))}
		default:
			return CompareResult{Message: fmt.Sprintf(
				"第 %d 行: 期望 %q, 实际 %q", i+1, truncate(e), truncate(a))}
		}
	}
	// Lengths differed but every compared line matched: only trailing blank
	// lines remained, which normalizeLines already strips. Treat as equal.
	return CompareResult{OK: true}
}

// normalizeLines splits into lines, trims trailing whitespace from each, and
// drops trailing blank lines.
func normalizeLines(b []byte) []string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t\v\f")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func truncate(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// trimForLog bounds a compiler or stderr dump before it is stored or shown.
func trimForLog(b []byte, max int) string {
	b = bytes.TrimRight(b, "\x00")
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "\n...(日志过长，已截断)"
}
