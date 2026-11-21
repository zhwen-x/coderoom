package client

import (
	"os"
	"path/filepath"
	"testing"

	"coderoom/internal/protocol"
)

func TestNormalizeAddr(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{"192.168.1.10:8080", "192.168.1.10:8080", false},
		{"192.168.1.10", "192.168.1.10:8080", false},
		{"http://192.168.1.10:8080", "192.168.1.10:8080", false},
		{"http://192.168.1.10:8080/", "192.168.1.10:8080", false},
		{"  192.168.1.10:9000  ", "192.168.1.10:9000", false},
		{"teacher-pc", "teacher-pc:8080", false},
		{"", "", true},
		{":8080", "", true},
	}
	for _, tc := range tests {
		got, err := NormalizeAddr(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("NormalizeAddr(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("NormalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLocalStoreSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	ls, err := OpenLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.SaveDraft(7, "cpp", "int main(){}"); err != nil {
		t.Fatal(err)
	}
	ls.QueueEvent(protocol.Event{Type: protocol.EvPasteLarge, Severity: protocol.SevWarn})

	// A crash, a reboot, or just closing the client must not lose work.
	reopened, err := OpenLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := reopened.State()
	if d := st.Drafts[7]; d == nil || d.Code != "int main(){}" || d.Language != "cpp" {
		t.Fatalf("draft lost across reopen: %+v", st.Drafts)
	}
	if len(st.PendingEvents) != 1 {
		t.Errorf("queued events lost across reopen: %d", len(st.PendingEvents))
	}
}

func TestDraftDirtyTracksUpload(t *testing.T) {
	ls, err := OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.SaveDraft(1, "cpp", "v1"); err != nil {
		t.Fatal(err)
	}
	if !ls.State().Drafts[1].Dirty() {
		t.Fatal("a freshly saved draft must be pending upload")
	}

	ls.MarkUploaded(1, HashCode("v1"))
	if ls.State().Drafts[1].Dirty() {
		t.Error("draft still dirty after the server acknowledged it")
	}

	// Editing again makes it pending once more.
	if err := ls.SaveDraft(1, "cpp", "v2"); err != nil {
		t.Fatal(err)
	}
	if !ls.State().Drafts[1].Dirty() {
		t.Error("edited draft should be pending upload again")
	}
}

func TestEventQueueReturnsOnFailure(t *testing.T) {
	ls, err := OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ls.QueueEvent(protocol.Event{Type: protocol.EvUSBInserted})
	}

	taken := ls.TakeEvents(2)
	if len(taken) != 2 || len(ls.State().PendingEvents) != 1 {
		t.Fatalf("TakeEvents(2) took %d, %d left", len(taken), len(ls.State().PendingEvents))
	}

	// A failed upload must put the evidence back, not drop it.
	ls.ReturnEvents(taken)
	if got := len(ls.State().PendingEvents); got != 3 {
		t.Errorf("after ReturnEvents: %d queued, want 3", got)
	}
}

func TestAdoptServerDraftsKeepsNewerLocalWork(t *testing.T) {
	ls, err := OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := ls.SaveDraft(1, "cpp", "本机较新的代码"); err != nil {
		t.Fatal(err)
	}
	localTime := ls.State().Drafts[1].UpdatedAt

	ls.AdoptServerDrafts([]protocol.Draft{
		// Older than what this machine has: must not clobber it.
		{ProblemID: 1, Language: "cpp", Code: "服务端的旧代码", UpdatedAt: localTime - 10_000},
		// A problem this machine has nothing for: adopt it.
		{ProblemID: 2, Language: "python", Code: "print(1)", UpdatedAt: localTime},
	})

	st := ls.State()
	if st.Drafts[1].Code != "本机较新的代码" {
		t.Errorf("newer local draft was overwritten by an older server copy: %q", st.Drafts[1].Code)
	}
	if st.Drafts[2] == nil || st.Drafts[2].Code != "print(1)" {
		t.Errorf("server draft not adopted for a problem with no local work")
	}
	// An adopted draft is already on the server, so it must not be re-uploaded.
	if st.Drafts[2].Dirty() {
		t.Error("adopted draft marked dirty")
	}
}

func TestExportAnswersWritesEverySourceFile(t *testing.T) {
	dir := t.TempDir()
	ls, err := OpenLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = ls.Update(func(st *LocalState) { st.StudentNo = "2023001"; st.Name = "张三" })
	_ = ls.SaveDraft(1, "cpp", "int main(){}")
	_ = ls.SaveDraft(2, "python", "print(1)")

	out, err := ls.ExportAnswers(dir, map[int64]string{1: "A+B 问题", 2: "判断奇偶"})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	// Two sources plus the metadata file.
	if len(entries) != 3 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("exported %v, want 2 sources + answers.json", names)
	}
	if _, err := os.Stat(filepath.Join(out, "answers.json")); err != nil {
		t.Errorf("metadata missing: %v", err)
	}

	found := map[string]bool{}
	for _, e := range entries {
		found[filepath.Ext(e.Name())] = true
	}
	if !found[".cpp"] || !found[".py"] {
		t.Errorf("language extensions wrong: %v", found)
	}
}

func TestSanitizeFileName(t *testing.T) {
	// Problem titles come from the teacher and become file names.
	for _, in := range []string{"a/b", `c\d`, "e:f", "g*h", "i?j", `k"l`, "m<n>o", "p|q", ""} {
		got := sanitizeFileName(in)
		for _, bad := range []rune{'/', '\\', ':', '*', '?', '"', '<', '>', '|'} {
			for _, r := range got {
				if r == bad {
					t.Errorf("sanitizeFileName(%q) = %q, still contains %q", in, got, bad)
				}
			}
		}
		if got == "" {
			t.Errorf("sanitizeFileName(%q) returned empty", in)
		}
	}
}

func TestHashCodeShape(t *testing.T) {
	// Agreement with the server is asserted in hash_consistency_test.go; this
	// only checks the shape the protocol expects.
	if got := HashCode("int main(){}"); len(got) != 64 {
		t.Fatalf("hash = %q, want 64 hex chars", got)
	}
	if HashCode("a") == HashCode("b") {
		t.Error("different inputs produced the same hash")
	}
	if HashCode("") == "" {
		t.Error("empty input must still hash")
	}
}

func TestSourceExt(t *testing.T) {
	for lang, want := range map[string]string{
		protocol.LangCPP: "cpp", protocol.LangC: "c", protocol.LangPython: "py",
		"unknown": "cpp",
	} {
		if got := SourceExt(lang); got != want {
			t.Errorf("SourceExt(%q) = %q, want %q", lang, got, want)
		}
	}
}
