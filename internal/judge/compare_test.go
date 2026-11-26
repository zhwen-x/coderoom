package judge

import "testing"

func TestCompareOutput(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		wantOK   bool
	}{
		{"exact", "3\n", "3\n", true},
		{"missing trailing newline", "3\n", "3", true},
		{"extra trailing newlines", "3\n", "3\n\n\n", true},
		{"crlf from windows build", "1 2\n3 4\n", "1 2\r\n3 4\r\n", true},
		{"trailing spaces ignored", "hello\n", "hello   \n", true},
		{"trailing tabs ignored", "a b\n", "a b\t\n", true},
		{"leading space matters", " x\n", "x\n", false},
		{"internal blank line matters", "a\n\nb\n", "a\nb\n", false},
		{"wrong value", "3\n", "4\n", false},
		{"short output", "1\n2\n", "1\n", false},
		{"long output", "1\n", "1\n2\n", false},
		{"both empty", "", "", true},
		{"expected empty actual blank lines", "", "\n\n", true},
		{"expected empty actual content", "", "x\n", false},
		{"multiline exact", "1\n2\n3\n", "1\n2\n3\n", true},
		{"case sensitive", "Yes\n", "yes\n", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareOutput([]byte(tc.expected), []byte(tc.actual), CompareOptions{})
			if got.OK != tc.wantOK {
				t.Fatalf("CompareOutput(%q, %q).OK = %v, want %v (msg=%q)",
					tc.expected, tc.actual, got.OK, tc.wantOK, got.Message)
			}
			if !got.OK && got.Message == "" {
				t.Error("mismatch reported with no diagnostic message")
			}
		})
	}
}

func TestNormalizeLinesDropsOnlyTrailingBlanks(t *testing.T) {
	got := normalizeLines([]byte("a\n\nb\n\n\n"))
	want := []string{"a", "", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

// A float problem where every correct answer is rejected is a worse failure
// than a lenient one: the teacher sees a room full of WA and has no idea why.
func TestFloatToleranceAcceptsRoundingAndStillRejectsWrong(t *testing.T) {
	const eps = 1e-6
	tests := []struct {
		name             string
		expected, actual string
		wantOK           bool
	}{
		{"末位舍入", "3.141593\n", "3.1415926\n", true},
		{"位数不同", "2.0\n", "2.000000\n", true},
		{"科学计数法", "1e3\n", "1000.0\n", true},
		{"一行多个数", "1.5 2.5 3.5\n", "1.4999999 2.5000001 3.5\n", true},
		{"超出容差", "1.0\n", "1.01\n", false},
		{"数值巨大时按相对误差", "1000000.0\n", "1000000.4\n", true},
		{"整数仍然严格", "7\n", "8\n", false},
		{"字段数不同", "1.0 2.0\n", "1.0\n", false},
		{"非数字部分必须一致", "x 1.0\n", "y 1.0\n", false},
		{"非数字部分相同则比数字", "ans 1.0\n", "ans 0.9999999\n", true},
		{"负零", "-0.0\n", "0.0\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CompareOutput([]byte(tc.expected), []byte(tc.actual),
				CompareOptions{FloatEpsilon: eps})
			if got.OK != tc.wantOK {
				t.Errorf("期望 OK=%v，得到 %v（%s）", tc.wantOK, got.OK, got.Message)
			}
		})
	}
}

// Without a tolerance the comparison must not drift into accepting numbers
// that merely look close: the default has to stay exact.
func TestWithoutToleranceFloatsAreExact(t *testing.T) {
	got := CompareOutput([]byte("3.141593\n"), []byte("3.1415926\n"), CompareOptions{})
	if got.OK {
		t.Error("未设置容差时，不同的小数被判为相同")
	}
}

// NaN and infinity come out of a buggy submission, not a correct one, and must
// not slip through a relative-error check.
func TestToleranceHandlesNonFiniteValues(t *testing.T) {
	opt := CompareOptions{FloatEpsilon: 1e-6}
	for _, tc := range []struct {
		expected, actual string
		wantOK           bool
	}{
		{"1.0\n", "nan\n", false},
		{"1.0\n", "inf\n", false},
		{"inf\n", "inf\n", true},
		{"nan\n", "nan\n", true},
	} {
		got := CompareOutput([]byte(tc.expected), []byte(tc.actual), opt)
		if got.OK != tc.wantOK {
			t.Errorf("%q vs %q: OK=%v，期望 %v", tc.expected, tc.actual, got.OK, tc.wantOK)
		}
	}
}
