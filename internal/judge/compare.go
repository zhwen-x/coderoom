package judge

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// CompareResult is the outcome of checking one test case's output.
type CompareResult struct {
	OK      bool
	Message string // first difference, for the teacher's eyes
}

// CompareOptions selects how strictly output is matched.
type CompareOptions struct {
	// FloatEpsilon, when positive, compares numbers numerically instead of as
	// text, accepting a difference within this tolerance. Zero keeps the exact
	// match, which is right for everything that is not a float problem.
	FloatEpsilon float64
}

// CompareOutput applies the default comparison rules:
//
//   - CRLF and CR are normalised to LF, so a Windows-built program that emits
//     "\r\n" is not marked wrong;
//   - trailing whitespace on each line is ignored;
//   - trailing blank lines at the end of the file are ignored.
//
// Everything else is a strict match unless a tolerance is set. Exact matching
// is the default on purpose: it is the only rule that cannot quietly accept a
// wrong answer, and a tolerance belongs to problems whose author chose one.
func CompareOutput(expected, actual []byte, opt CompareOptions) CompareResult {
	exp := normalizeLines(expected)
	act := normalizeLines(actual)

	if len(exp) == len(act) {
		same := true
		for i := range exp {
			if !sameLine(exp[i], act[i], opt) {
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
		if eOK == aOK && sameLine(e, a, opt) {
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

// sameLine compares one line, field by field when a tolerance is in force.
//
// Fields that both parse as numbers are compared numerically; anything else
// still has to match exactly, so a problem whose output mixes labels and
// values does not start accepting the wrong labels.
func sameLine(expected, actual string, opt CompareOptions) bool {
	if expected == actual {
		return true
	}
	if opt.FloatEpsilon <= 0 {
		return false
	}
	ef, af := strings.Fields(expected), strings.Fields(actual)
	if len(ef) != len(af) {
		return false
	}
	for i := range ef {
		if ef[i] == af[i] {
			continue
		}
		e, err1 := strconv.ParseFloat(ef[i], 64)
		a, err2 := strconv.ParseFloat(af[i], 64)
		if err1 != nil || err2 != nil {
			return false
		}
		if !closeEnough(e, a, opt.FloatEpsilon) {
			return false
		}
	}
	return true
}

// closeEnough accepts a difference that is small either absolutely or
// relatively. Absolute alone would reject correct answers to a problem whose
// values run to millions; relative alone would reject them near zero.
func closeEnough(expected, actual, eps float64) bool {
	if math.IsNaN(expected) || math.IsNaN(actual) {
		return math.IsNaN(expected) && math.IsNaN(actual)
	}
	if math.IsInf(expected, 0) || math.IsInf(actual, 0) {
		return expected == actual
	}
	diff := math.Abs(expected - actual)
	if diff <= eps {
		return true
	}
	scale := math.Abs(expected)
	return scale > 0 && diff/scale <= eps
}
