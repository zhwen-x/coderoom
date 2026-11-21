package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

type harness struct {
	t      *testing.T
	srv    *Server
	ts     *httptest.Server
	st     *store.Store
	token  string // student session token
	cancel context.CancelFunc
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	cfg := DefaultConfig()
	cfg.Workers = 1
	cfg.AdminToken = "test-admin-token"

	srv, err := New(st, cfg, dir, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())

	h := &harness{t: t, srv: srv, ts: ts, st: st}
	t.Cleanup(func() {
		ts.Close()
		st.Close()
	})
	return h
}

// startWorkers runs the judge pool for tests that need real judging.
func (h *harness) startWorkers() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.t.Cleanup(cancel)
	go h.srv.judgeWorker(ctx, "test-worker")
}

func (h *harness) do(method, path string, body any, admin bool) (int, []byte) {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if admin {
		req.Header.Set("X-Admin-Token", "test-admin-token")
	} else if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, raw
}

func (h *harness) mustJSON(status int, raw []byte, out any) {
	h.t.Helper()
	if status < 200 || status >= 300 {
		h.t.Fatalf("HTTP %d: %s", status, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("decode %s: %v", raw, err)
		}
	}
}

// seedPractice creates a judgeable problem and an open practice session.
func (h *harness) seedPractice() (problemID, examID int64) {
	h.t.Helper()
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{
		"title": "A+B", "time_limit_ms": 2000, "memory_limit_mb": 256,
		"languages": []string{"cpp", "python"},
	}, true)
	h.mustJSON(status, raw, &p)

	status, raw = h.do(http.MethodPut, "/api/teacher/problems/1/testcases", []map[string]any{
		{"input": "1 2\n", "output": "3\n", "score": 10},
		{"input": "4 5\n", "output": "9\n", "score": 10},
	}, true)
	h.mustJSON(status, raw, nil)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "练习", "type": "practice", "show_results": true, "allow_rejudge": true,
		"state": "published",
		"items": []map[string]any{{"problem_id": p.ID, "score": 20}},
	}, true)
	h.mustJSON(status, raw, &e)
	return p.ID, e.ID
}

func (h *harness) login(examID int64, no, name, passcode string) protocol.LoginResponse {
	h.t.Helper()
	var out protocol.LoginResponse
	status, raw := h.do(http.MethodPost, "/api/client/login", protocol.LoginRequest{
		ExamID: examID, StudentNo: no, Name: name, Passcode: passcode,
		ClientTime: time.Now().UnixMilli(),
	}, false)
	h.mustJSON(status, raw, &out)
	h.token = out.Token
	return out
}

func TestTeacherAPIRequiresToken(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{
		"/api/teacher/problems", "/api/teacher/exams", "/api/teacher/system",
	} {
		// Without a token the teacher API would hand the test data and every
		// student's code to any machine on the classroom LAN.
		if status, _ := h.do(http.MethodGet, path, nil, false); status != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, status)
		}
		if status, _ := h.do(http.MethodGet, path, nil, true); status != http.StatusOK {
			t.Errorf("GET %s with token = %d, want 200", path, status)
		}
	}
}

func TestClientCannotReachTeacherAPIWithSessionToken(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedPractice()
	h.login(examID, "2023", "张三", "")

	req, _ := http.NewRequest(http.MethodGet, h.ts.URL+"/api/teacher/problems", nil)
	req.Header.Set("X-Admin-Token", h.token) // a student's own token
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("student token accepted by teacher API: %d", res.StatusCode)
	}
}

func TestExamWithheldUntilStarted(t *testing.T) {
	h := newHarness(t)
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{
		"title": "密题", "description_md": "这是考试题面，开考前绝不能下发",
	}, true)
	h.mustJSON(status, raw, &p)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "期末考试", "type": "exam", "state": "published",
		"items": []map[string]any{{"problem_id": p.ID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	h.login(e.ID, "2023", "张三", "")

	var paper protocol.PaperResponse
	status, raw = h.do(http.MethodGet, "/api/client/paper", nil, false)
	h.mustJSON(status, raw, &paper)

	if !paper.Locked {
		t.Fatal("paper should be locked before the exam starts")
	}
	if len(paper.Items) != 1 {
		t.Fatalf("student should still see how many problems there are")
	}
	if paper.Items[0].Problem != nil {
		t.Fatal("problem statement leaked before the exam started")
	}
	// The statement must not appear anywhere in the response body.
	if strings.Contains(string(raw), "绝不能下发") {
		t.Fatal("statement text present in a locked paper response")
	}

	// After the teacher starts it, the statement is released.
	status, raw = h.do(http.MethodPost, "/api/teacher/exams/"+itoa(e.ID)+"/state",
		map[string]any{"state": "running"}, true)
	h.mustJSON(status, raw, nil)

	status, raw = h.do(http.MethodGet, "/api/client/paper", nil, false)
	h.mustJSON(status, raw, &paper)
	if paper.Locked || paper.Items[0].Problem == nil {
		t.Fatal("statement not released after the exam started")
	}
}

func TestExamResultsHiddenUntilReleased(t *testing.T) {
	h := newHarness(t)
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{"title": "P"}, true)
	h.mustJSON(status, raw, &p)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "考试", "type": "exam", "state": "running", "show_results": true,
		"items": []map[string]any{{"problem_id": p.ID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)
	// An exam must never be created with results already visible, whatever
	// the request asked for.
	if e.ShowResults {
		t.Fatal("exam created with results visible")
	}

	h.login(e.ID, "2023", "张三", "")
	var res protocol.ResultsResponse
	status, raw = h.do(http.MethodGet, "/api/client/results", nil, false)
	h.mustJSON(status, raw, &res)
	if res.Visible {
		t.Error("exam results visible before release")
	}

	status, raw = h.do(http.MethodPost, "/api/teacher/exams/"+itoa(e.ID)+"/release",
		map[string]any{"show": true}, true)
	h.mustJSON(status, raw, nil)

	status, raw = h.do(http.MethodGet, "/api/client/results", nil, false)
	h.mustJSON(status, raw, &res)
	if !res.Visible {
		t.Error("results still hidden after the teacher released them")
	}
}

func TestRosterEnforcement(t *testing.T) {
	h := newHarness(t)
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{"title": "P"}, true)
	h.mustJSON(status, raw, &p)

	status, raw = h.do(http.MethodPost, "/api/teacher/students/import", map[string]any{
		"csv": "学号,姓名,班级\n2023001,张三,计算机2301\n2023002,李四,计算机2301\n",
	}, true)
	h.mustJSON(status, raw, nil)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "考试", "type": "practice", "state": "published", "roster_required": true,
		"items": []map[string]any{{"problem_id": p.ID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	tests := []struct {
		name, no, student string
		wantOK            bool
	}{
		{"名单内且姓名匹配", "2023001", "张三", true},
		{"姓名不符", "2023002", "王五", false},
		{"不在名单", "9999999", "赵六", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := h.do(http.MethodPost, "/api/client/login", protocol.LoginRequest{
				ExamID: e.ID, StudentNo: tc.no, Name: tc.student,
			}, false)
			ok := status == http.StatusOK
			if ok != tc.wantOK {
				t.Fatalf("status %d (%s), wantOK=%v", status, raw, tc.wantOK)
			}
			if ok {
				var out protocol.LoginResponse
				_ = json.Unmarshal(raw, &out)
				if out.Class != "计算机2301" {
					t.Errorf("class not filled from roster: %q", out.Class)
				}
			}
		})
	}
}

func TestPasscodeRequired(t *testing.T) {
	h := newHarness(t)
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{"title": "P"}, true)
	h.mustJSON(status, raw, &p)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "考试", "type": "practice", "state": "published", "passcode": "s3cret",
		"items": []map[string]any{{"problem_id": p.ID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	// The passcode itself must never be advertised to an anonymous client.
	status, raw = h.do(http.MethodGet, "/api/client/exams", nil, false)
	h.mustJSON(status, raw, nil)
	if strings.Contains(string(raw), "s3cret") {
		t.Fatal("passcode leaked in the public exam list")
	}
	var list protocol.ExamListResponse
	_ = json.Unmarshal(raw, &list)
	if len(list.Exams) != 1 || !list.Exams[0].PasscodeRequired {
		t.Fatalf("exam list should flag that a passcode is required: %+v", list.Exams)
	}

	if status, _ := h.do(http.MethodPost, "/api/client/login", protocol.LoginRequest{
		ExamID: e.ID, StudentNo: "2023", Name: "张三", Passcode: "wrong",
	}, false); status != http.StatusForbidden {
		t.Errorf("wrong passcode accepted: %d", status)
	}
	if status, _ := h.do(http.MethodPost, "/api/client/login", protocol.LoginRequest{
		ExamID: e.ID, StudentNo: "2023", Name: "张三", Passcode: "s3cret",
	}, false); status != http.StatusOK {
		t.Errorf("correct passcode rejected: %d", status)
	}
}

func TestSubmitRejectsProblemsNotOnThePaper(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedPractice()

	// A second problem exists but is not on this student's paper.
	var other store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{"title": "别人的题"}, true)
	h.mustJSON(status, raw, &other)

	h.login(examID, "2023", "张三", "")
	status, raw = h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: other.ID, Language: "cpp", Code: "int main(){}",
	}, false)
	if status != http.StatusForbidden {
		t.Errorf("submitting an off-paper problem = %d (%s), want 403", status, raw)
	}
}

func TestSubmitRejectsTamperedHash(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()
	h.login(examID, "2023", "张三", "")

	status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: problemID, Language: "cpp", Code: "int main(){}",
		Hash: "0000000000000000000000000000000000000000000000000000000000000000",
	}, false)
	if status != http.StatusBadRequest {
		t.Errorf("tampered body accepted: %d (%s)", status, raw)
	}
}

func TestSnapshotSkipsUnchangedCode(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()
	h.login(examID, "2023", "张三", "")

	send := func(code string) protocol.SnapshotResponse {
		var out protocol.SnapshotResponse
		status, raw := h.do(http.MethodPost, "/api/client/snapshot", protocol.SnapshotRequest{
			Items: []protocol.SnapshotItem{{
				ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
			}},
		}, false)
		h.mustJSON(status, raw, &out)
		return out
	}

	if got := send("v1"); len(got.Stored) != 1 {
		t.Fatalf("first snapshot not stored: %+v", got)
	}
	if got := send("v1"); len(got.Skipped) != 1 || len(got.Stored) != 0 {
		t.Fatalf("identical snapshot should be skipped: %+v", got)
	}
	if got := send("v2"); len(got.Stored) != 1 {
		t.Fatalf("changed snapshot should be stored: %+v", got)
	}
}

func TestHeartbeatDeliversPolicyWhenStale(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedPractice()
	login := h.login(examID, "2023", "张三", "")

	beat := func(version int) protocol.HeartbeatResponse {
		var out protocol.HeartbeatResponse
		status, raw := h.do(http.MethodPost, "/api/client/heartbeat",
			protocol.HeartbeatRequest{PolicyVersion: version}, false)
		h.mustJSON(status, raw, &out)
		return out
	}

	if got := beat(login.PolicyVersion); got.Policy != nil {
		t.Error("policy resent to an up-to-date client")
	}
	// This is the mechanism that fixes the classroom-software failure mode:
	// a client that missed a change converges on its own, with no push.
	if got := beat(0); got.Policy == nil {
		t.Fatal("stale client did not receive the current policy")
	}
	if got := beat(0); got.ServerTime == 0 {
		t.Error("every response must carry server time; the client clock is untrusted")
	}
}

func TestClosedExamRejectsWrites(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()
	h.login(examID, "2023", "张三", "")

	status, raw := h.do(http.MethodPost, "/api/teacher/exams/"+itoa(examID)+"/state",
		map[string]any{"state": "ended"}, true)
	h.mustJSON(status, raw, nil)

	for _, tc := range []struct {
		name, path string
		body       any
	}{
		{"提交", "/api/client/submit", protocol.SubmitRequest{
			ProblemID: problemID, Language: "cpp", Code: "int main(){}"}},
		{"快照", "/api/client/snapshot", protocol.SnapshotRequest{
			Items: []protocol.SnapshotItem{{ProblemID: problemID, Language: "cpp", Code: "x"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, raw := h.do(http.MethodPost, tc.path, tc.body, false); status != http.StatusForbidden {
				t.Errorf("accepted after the exam ended: %d (%s)", status, raw)
			}
		})
	}
}

func TestPracticeSubmitIsJudgedAndReturned(t *testing.T) {
	h := newHarness(t)
	if !h.srv.judger.Toolchain().Available()[protocol.LangCPP] {
		t.Skip("no C++ toolchain on this machine")
	}
	h.startWorkers()

	problemID, examID := h.seedPractice()
	h.login(examID, "2023", "张三", "")

	code := "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<std::endl;}"
	var sub protocol.SubmitResponse
	status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
	}, false)
	h.mustJSON(status, raw, &sub)
	if !sub.Queued {
		t.Fatal("practice submission was not queued")
	}

	deadline := time.Now().Add(30 * time.Second)
	var res protocol.ResultsResponse
	for time.Now().Before(deadline) {
		status, raw = h.do(http.MethodGet, "/api/client/results", nil, false)
		h.mustJSON(status, raw, &res)
		if len(res.Results) > 0 && res.Results[0].Verdict != protocol.VerdictPending {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(res.Results) == 0 {
		t.Fatal("no result returned to the student")
	}
	if res.Results[0].Verdict != protocol.VerdictAC {
		t.Fatalf("verdict = %s (%s)", res.Results[0].Verdict, res.Results[0].CompileLog)
	}
	if res.Results[0].Score != 20 || res.Results[0].MaxScore != 20 {
		t.Errorf("score = %d/%d, want 20/20", res.Results[0].Score, res.Results[0].MaxScore)
	}
	if len(res.Results[0].Cases) != 2 {
		t.Errorf("got %d case results, want 2", len(res.Results[0].Cases))
	}
}

func TestScoresCSVHasBOMForExcel(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedPractice()
	h.login(examID, "2023001", "张三", "")

	req, _ := http.NewRequest(http.MethodGet,
		h.ts.URL+"/api/teacher/exams/"+itoa(examID)+"/scores.csv", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	// Excel on Windows reads a BOM-less UTF-8 CSV as ANSI and mangles every
	// Chinese name, so the BOM is a requirement.
	if len(raw) < 3 || raw[0] != 0xEF || raw[1] != 0xBB || raw[2] != 0xBF {
		t.Fatalf("CSV missing UTF-8 BOM: % x", raw[:min(8, len(raw))])
	}
	if !strings.Contains(string(raw), "张三") {
		t.Errorf("student missing from the CSV:\n%s", raw)
	}
}

func TestRosterCSVParsing(t *testing.T) {
	tests := []struct {
		name, input string
		want        int
	}{
		{"逗号分隔", "2023001,张三,计算2301\n2023002,李四,计算2301", 2},
		{"制表符(Excel 粘贴)", "2023001\t张三\t计算2301", 1},
		{"全角逗号", "2023001，张三，计算2301", 1},
		{"跳过表头", "学号,姓名,班级\n2023001,张三,计算2301", 1},
		{"忽略空行", "2023001,张三\n\n\n2023002,李四\n", 2},
		{"跳过残缺行", "2023001,张三\nonly-one-field\n2023002,李四", 2},
		{"带 BOM", "\ufeff2023001,张三", 1},
		{"无班级", "2023001,张三", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := parseRosterCSV(tc.input)
			if len(got) != tc.want {
				t.Fatalf("parsed %d rows, want %d: %+v", len(got), tc.want, got)
			}
			for _, s := range got {
				if s.StudentNo == "" || s.Name == "" {
					t.Errorf("incomplete row survived: %+v", s)
				}
			}
		})
	}
}

func TestConfigPersistsAdminToken(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	first, err := New(st, DefaultConfig(), dir, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	tok := first.Config().AdminToken
	if tok == "" {
		t.Fatal("no admin token generated")
	}
	if _, err := os.Stat(dir + "/config.json"); err != nil {
		t.Fatalf("config not written: %v", err)
	}

	// A restart must keep the token, or the teacher's open tab breaks every
	// time the server is restarted mid-session.
	second, err := New(st, LoadConfig(dir), dir, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if second.Config().AdminToken != tok {
		t.Error("admin token changed across restart")
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
