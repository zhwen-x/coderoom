package store

import (
	"context"
	"testing"

	"coderoom/internal/protocol"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedProblem(t *testing.T, s *Store, title string, cases int) int64 {
	t.Helper()
	ctx := context.Background()
	p := &Problem{Title: title, TimeLimitMS: 1000, MemoryLimitMB: 256}
	id, err := s.CreateProblem(ctx, p)
	if err != nil {
		t.Fatalf("create problem: %v", err)
	}
	in := make([]TestcaseInput, 0, cases)
	for i := 0; i < cases; i++ {
		in = append(in, TestcaseInput{Input: "1 2\n", Output: "3\n", Score: 10})
	}
	if cases > 0 {
		if err := s.ReplaceTestcases(ctx, id, in); err != nil {
			t.Fatalf("replace testcases: %v", err)
		}
	}
	return id
}

func TestProblemRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p := &Problem{
		Title:         "A+B",
		DescriptionMD: "求和",
		Samples:       []protocol.Sample{{Input: "1 2", Output: "3"}},
		Tags:          []string{"入门", "顺序"},
		Languages:     []string{"cpp", "python"},
	}
	id, err := s.CreateProblem(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := s.GetProblem(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Title != "A+B" || len(got.Samples) != 1 || got.Samples[0].Output != "3" {
		t.Errorf("round trip lost data: %+v", got)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "入门" {
		t.Errorf("tags round trip: %v", got.Tags)
	}
	// Defaults must be applied for fields the teacher left blank.
	if got.TimeLimitMS != 1000 || got.MemoryLimitMB != 256 || got.ScoreMode != ScoreModeSum {
		t.Errorf("defaults not applied: %+v", got)
	}
}

func TestReplaceTestcasesIsAFullReplacement(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	id := seedProblem(t, s, "P", 3)

	if cases, _ := s.ListTestcases(ctx, id); len(cases) != 3 {
		t.Fatalf("got %d cases, want 3", len(cases))
	}
	// Replacing with fewer cases must drop the extras, not merge.
	if err := s.ReplaceTestcases(ctx, id, []TestcaseInput{{Input: "x", Output: "y", Score: 5}}); err != nil {
		t.Fatal(err)
	}
	cases, _ := s.ListTestcases(ctx, id)
	if len(cases) != 1 || cases[0].Score != 5 || cases[0].Idx != 1 {
		t.Fatalf("replacement left stale cases: %+v", cases)
	}

	p, _ := s.GetProblem(ctx, id)
	if p.CaseCount != 1 || p.CaseScore != 5 {
		t.Errorf("derived counts stale: count=%d score=%d", p.CaseCount, p.CaseScore)
	}
}

func TestExamGatesByState(t *testing.T) {
	now := int64(1_000_000)
	start := now - 1000
	future := now + 60_000

	cases := []struct {
		name     string
		exam     Exam
		unlocked bool
	}{
		{"练习已发布即开放", Exam{Type: protocol.ExamTypePractice, State: protocol.ExamStatePublished}, true},
		{"练习草稿不开放", Exam{Type: protocol.ExamTypePractice, State: protocol.ExamStateDraft}, false},
		{"考试已发布但未开考", Exam{Type: protocol.ExamTypeExam, State: protocol.ExamStatePublished}, false},
		{"考试进行中且已到时间", Exam{Type: protocol.ExamTypeExam, State: protocol.ExamStateRunning, StartAt: &start}, true},
		{"考试进行中但未到开始时间", Exam{Type: protocol.ExamTypeExam, State: protocol.ExamStateRunning, StartAt: &future}, false},
		{"考试已结束", Exam{Type: protocol.ExamTypeExam, State: protocol.ExamStateEnded}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := tc.exam.PaperUnlocked(now)
			if got != tc.unlocked {
				t.Fatalf("PaperUnlocked = %v (%s), want %v", got, reason, tc.unlocked)
			}
			if !got && reason == "" {
				t.Error("locked paper with no explanation for the student")
			}
		})
	}
}

func TestExamDeadlineAndSubmissionWindow(t *testing.T) {
	start := int64(1_000_000)
	e := Exam{
		Type: protocol.ExamTypeExam, State: protocol.ExamStateRunning,
		StartAt: &start, DurationMin: 60,
	}
	d := e.Deadline()
	if d == nil || *d != start+60*60_000 {
		t.Fatalf("deadline derived from duration = %v", d)
	}
	if ok, _ := e.AcceptsSubmissions(start + 1000); !ok {
		t.Error("should accept inside the window")
	}
	if ok, reason := e.AcceptsSubmissions(*d + 1); ok {
		t.Error("must reject after the deadline")
	} else if reason == "" {
		t.Error("rejection needs a reason")
	}

	// An explicit end time wins over the derived one.
	end := start + 1000
	e.EndAt = &end
	if got := e.Deadline(); got == nil || *got != end {
		t.Errorf("explicit end_at should win, got %v", got)
	}
}

func TestLoginResumesSessionAndFlagsMachineChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProblem(t, s, "P", 2)

	e := &Exam{Name: "练习", Type: protocol.ExamTypePractice, State: protocol.ExamStatePublished}
	if err := s.CreateExam(ctx, e, []PaperItemInput{{ProblemID: pid, Score: 20}}); err != nil {
		t.Fatal(err)
	}

	first, err := s.Login(ctx, &Session{
		ExamID: e.ID, StudentNo: "2023", Name: "张三", MachineName: "PC-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.PrevMachine != "" {
		t.Error("first login should not report a machine change")
	}

	// Re-login on the same machine resumes rather than creating a second row.
	same, err := s.Login(ctx, &Session{
		ExamID: e.ID, StudentNo: "2023", Name: "张三", MachineName: "PC-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if same.Session.ID != first.Session.ID {
		t.Errorf("re-login created a new session %d (was %d)", same.Session.ID, first.Session.ID)
	}
	if same.Session.Token == first.Session.Token {
		t.Error("token should be rotated on re-login")
	}
	// The old token must stop working, or a copied token would stay valid.
	if _, err := s.SessionByToken(ctx, first.Session.Token); err == nil {
		t.Error("the previous token is still accepted")
	}

	moved, err := s.Login(ctx, &Session{
		ExamID: e.ID, StudentNo: "2023", Name: "张三", MachineName: "PC-99",
	})
	if err != nil {
		t.Fatal(err)
	}
	if moved.PrevMachine != "PC-01" {
		t.Errorf("machine change not reported, got %q", moved.PrevMachine)
	}

	if list, _ := s.ListSessions(ctx, e.ID); len(list) != 1 {
		t.Errorf("got %d sessions for one student, want 1", len(list))
	}
}

func TestPaperForStudentPrefersPerStudentPaper(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a := seedProblem(t, s, "班级卷题", 1)
	b := seedProblem(t, s, "个人卷题", 1)

	e := &Exam{Name: "考试", Type: protocol.ExamTypeExam, State: protocol.ExamStateRunning}
	if err := s.CreateExam(ctx, e, []PaperItemInput{{ProblemID: a, Score: 10}}); err != nil {
		t.Fatal(err)
	}

	got, err := s.PaperForStudent(ctx, e.ID, "2023")
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != "class" || got.Items[0].ProblemID != a {
		t.Fatalf("expected the class paper, got %+v", got)
	}

	_ = b // a per-student paper is generated in M2; the lookup above is what it will hit
}

func TestSubmissionSnapshotsAccumulateAndDedup(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProblem(t, s, "P", 2)
	e := &Exam{Name: "练习", Type: protocol.ExamTypePractice, State: protocol.ExamStatePublished}
	if err := s.CreateExam(ctx, e, []PaperItemInput{{ProblemID: pid, Score: 20}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Login(ctx, &Session{ExamID: e.ID, StudentNo: "2023", Name: "张三"})
	if err != nil {
		t.Fatal(err)
	}
	sess := res.Session

	for i, code := range []string{"v1", "v2", "v3"} {
		sub := &Submission{
			SessionID: sess.ID, ExamID: e.ID, StudentNo: "2023",
			ProblemID: pid, Language: "cpp", Kind: protocol.KindSnapshot,
		}
		if err := s.SaveSubmission(ctx, sub, code); err != nil {
			t.Fatal(err)
		}
		if sub.Seq != i+1 {
			t.Errorf("seq = %d, want %d", sub.Seq, i+1)
		}
	}

	// LatestHash is what lets the server skip an unchanged snapshot.
	h, err := s.LatestHash(ctx, sess.ID, pid)
	if err != nil {
		t.Fatal(err)
	}
	if h != HashCode("v3") {
		t.Errorf("LatestHash did not track the newest snapshot")
	}

	// Every version is kept: that archive is the editing timeline.
	tl, err := s.SnapshotTimeline(ctx, sess.ID, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(tl) != 3 {
		t.Errorf("timeline has %d entries, want 3", len(tl))
	}

	drafts, err := s.LatestDrafts(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].Code != "v3" {
		t.Errorf("draft recovery returned %+v", drafts)
	}
}

func TestJudgeQueueClaimIsExclusiveAndRecoverable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProblem(t, s, "P", 1)
	e := &Exam{Name: "练习", Type: protocol.ExamTypePractice, State: protocol.ExamStatePublished}
	if err := s.CreateExam(ctx, e, []PaperItemInput{{ProblemID: pid, Score: 10}}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Login(ctx, &Session{ExamID: e.ID, StudentNo: "2023", Name: "张三"})

	sub := &Submission{
		SessionID: res.Session.ID, ExamID: e.ID, StudentNo: "2023",
		ProblemID: pid, Language: "cpp", Kind: protocol.KindFinal,
	}
	if err := s.SaveSubmission(ctx, sub, "code"); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, sub.ID, 5); err != nil {
		t.Fatal(err)
	}

	task, err := s.ClaimTask(ctx, "w1")
	if err != nil || task == nil {
		t.Fatalf("claim: task=%v err=%v", task, err)
	}
	// A second worker must not get the same task.
	if again, err := s.ClaimTask(ctx, "w2"); err != nil || again != nil {
		t.Fatalf("second claim got task=%v err=%v, want nil", again, err)
	}

	// Simulating a crash: the task is stuck in 'running' and must be requeued.
	n, err := s.RequeueStaleTasks(ctx)
	if err != nil || n != 1 {
		t.Fatalf("requeue returned %d, %v", n, err)
	}
	if recovered, err := s.ClaimTask(ctx, "w3"); err != nil || recovered == nil {
		t.Fatalf("task not recoverable after restart: %v %v", recovered, err)
	}
}

func TestExamScoresKeepsBestFinalPerProblem(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	pid := seedProblem(t, s, "P", 3)
	e := &Exam{Name: "练习", Type: protocol.ExamTypePractice, State: protocol.ExamStatePublished, AllowRejudge: true}
	if err := s.CreateExam(ctx, e, []PaperItemInput{{ProblemID: pid, Score: 25}}); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Login(ctx, &Session{ExamID: e.ID, StudentNo: "2023", Name: "张三"})

	// Three attempts: 10, then 30 (best), then 20. The best must win even
	// though it is not the most recent.
	for _, score := range []int{10, 30, 20} {
		sub := &Submission{
			SessionID: res.Session.ID, ExamID: e.ID, StudentNo: "2023",
			ProblemID: pid, Language: "cpp", Kind: protocol.KindFinal,
		}
		if err := s.SaveSubmission(ctx, sub, "code-"+string(rune('a'+score))); err != nil {
			t.Fatal(err)
		}
		if err := s.SaveJudgeResult(ctx, &JudgeResult{
			SubmissionID: sub.ID, Verdict: protocol.VerdictWA,
			Score: score, MaxScore: 30,
		}); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := s.ExamScores(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d score rows, want 1", len(rows))
	}
	if rows[0].Score != 30 {
		t.Errorf("best score = %d, want 30", rows[0].Score)
	}
}

func TestEventsAreRecordedWithBothClocks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	examID := int64(7)
	if err := s.LogEvent(ctx, nil, &examID, "2023", protocol.Event{
		Type:       protocol.EvUSBInserted,
		Severity:   protocol.SevAlert,
		ClientTime: 123,
		Payload:    map[string]any{"drive": "E:"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListEvents(ctx, examID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d events, want 1", len(rows))
	}
	ev := rows[0]
	if ev.ClientTime != 123 {
		t.Error("client-reported time not preserved")
	}
	if ev.ServerTime == 0 {
		t.Error("server time must always be stamped; the client clock is untrusted")
	}
	if ev.Payload["drive"] != "E:" {
		t.Errorf("payload lost: %+v", ev.Payload)
	}
}

func TestSanitizeRejectsPathTraversal(t *testing.T) {
	// Student numbers come from an untrusted client and become path elements.
	for _, in := range []string{"../../etc", "a/b", `c\d`, "..", ""} {
		got := sanitize(in)
		for _, bad := range []string{"/", `\`, ".."} {
			if got == bad || (len(got) >= 2 && got[:2] == "..") {
				t.Errorf("sanitize(%q) = %q, still traversal-capable", in, got)
			}
		}
		if got == "" {
			t.Errorf("sanitize(%q) returned empty", in)
		}
	}
}
