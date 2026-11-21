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
			got := CompareOutput([]byte(tc.expected), []byte(tc.actual))
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
