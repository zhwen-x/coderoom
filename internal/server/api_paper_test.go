package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

// seedBank creates problems spread across difficulties, all with test data.
func (h *harness) seedBank(t *testing.T) {
	t.Helper()
	spec := []struct {
		title      string
		difficulty int
		tags       []string
	}{
		{"顺序A", 1, []string{"顺序结构"}},
		{"顺序B", 1, []string{"顺序结构"}},
		{"分支A", 2, []string{"分支"}},
		{"分支B", 2, []string{"分支"}},
		{"循环A", 3, []string{"循环"}},
		{"循环B", 3, []string{"循环"}},
	}
	for i, p := range spec {
		var got store.Problem
		status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{
			"title": p.title, "difficulty": p.difficulty, "tags": p.tags,
			"languages": []string{"cpp"},
		}, true)
		h.mustJSON(status, raw, &got)
		status, raw = h.do(http.MethodPut,
			"/api/teacher/problems/"+itoa(got.ID)+"/testcases", []map[string]any{
				{"input": "1\n", "output": "1\n", "score": 10},
			}, true)
		h.mustJSON(status, raw, nil)
		_ = i
	}
}

func (h *harness) saveTemplate(t *testing.T, tpl map[string]any) store.PaperTemplate {
	t.Helper()
	var got store.PaperTemplate
	status, raw := h.do(http.MethodPost, "/api/teacher/templates", tpl, true)
	h.mustJSON(status, raw, &got)
	return got
}

func TestPerStudentPapersAreDrawnOnLogin(t *testing.T) {
	h := newHarness(t)
	h.seedBank(t)

	tpl := h.saveTemplate(t, map[string]any{
		"name": "随机卷",
		"items": []map[string]any{
			{"pick_type": "filter", "score": 50,
				"filter": map[string]any{"min_difficulty": 1, "max_difficulty": 1}},
			{"pick_type": "filter", "score": 50,
				"filter": map[string]any{"min_difficulty": 3, "max_difficulty": 3}},
		},
	})
	if tpl.TotalScore != 100 {
		t.Errorf("total score = %d, want 100", tpl.TotalScore)
	}

	var exam store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "随机考试", "type": "exam", "state": "running",
		"paper_template_id": tpl.ID, "per_student_paper": true,
	}, true)
	h.mustJSON(status, raw, &exam)

	fetch := func(no, name string) protocol.PaperResponse {
		h.login(exam.ID, no, name, "")
		var paper protocol.PaperResponse
		status, raw := h.do(http.MethodGet, "/api/client/paper", nil, false)
		h.mustJSON(status, raw, &paper)
		return paper
	}

	a := fetch("2023001", "张三")
	b := fetch("2023002", "李四")

	for _, p := range []protocol.PaperResponse{a, b} {
		if len(p.Items) != 2 {
			t.Fatalf("paper has %d problems, want 2", len(p.Items))
		}
		if p.Items[0].Problem == nil {
			t.Fatal("问题正文未下发")
		}
		if p.Items[0].ProblemID == p.Items[1].ProblemID {
			t.Error("同一份试卷里出现了重复题目")
		}
	}

	// Difficulty is pinned per slot, which is what keeps per-student papers
	// comparable; only the specific problem differs.
	ctx := context.Background()
	diff := func(id int64) int {
		p, err := h.st.GetProblem(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return p.Difficulty
	}
	if diff(a.Items[0].ProblemID) != 1 || diff(b.Items[0].ProblemID) != 1 {
		t.Error("第一题不是难度 1")
	}
	if diff(a.Items[1].ProblemID) != 3 || diff(b.Items[1].ProblemID) != 3 {
		t.Error("第二题不是难度 3")
	}

	// Re-login must hand back the same paper, or a client restart would
	// reshuffle a student's questions mid-exam.
	again := fetch("2023001", "张三")
	for i := range again.Items {
		if again.Items[i].ProblemID != a.Items[i].ProblemID {
			t.Fatalf("重新登录后试卷变了: %v -> %v", a.Items, again.Items)
		}
	}
}

func TestClassPaperFromTemplateIsSharedByEveryone(t *testing.T) {
	h := newHarness(t)
	h.seedBank(t)

	tpl := h.saveTemplate(t, map[string]any{
		"name": "全班同卷",
		"items": []map[string]any{
			{"pick_type": "filter", "score": 100, "filter": map[string]any{}},
		},
	})
	var exam store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "统一卷考试", "type": "practice", "state": "published",
		"paper_template_id": tpl.ID, "per_student_paper": false,
	}, true)
	h.mustJSON(status, raw, &exam)

	seen := map[int64]bool{}
	for _, no := range []string{"2023001", "2023002", "2023003"} {
		h.login(exam.ID, no, "学生", "")
		var paper protocol.PaperResponse
		status, raw := h.do(http.MethodGet, "/api/client/paper", nil, false)
		h.mustJSON(status, raw, &paper)
		seen[paper.Items[0].ProblemID] = true
	}
	if len(seen) != 1 {
		t.Errorf("班级卷应当人人相同，实际出现了 %d 种题目", len(seen))
	}
}

func TestTemplatePreviewReportsImpossibleRules(t *testing.T) {
	h := newHarness(t)
	h.seedBank(t)

	tpl := h.saveTemplate(t, map[string]any{
		"name": "坏模板",
		"items": []map[string]any{
			{"pick_type": "filter", "score": 50,
				"filter": map[string]any{"tags": []string{"根本不存在的知识点"}}},
		},
	})

	var preview templatePreview
	status, raw := h.do(http.MethodPost,
		"/api/teacher/templates/"+itoa(tpl.ID)+"/preview", nil, true)
	h.mustJSON(status, raw, &preview)

	if preview.Items[0].Candidates != 0 {
		t.Errorf("候选题数 = %d，应为 0", preview.Items[0].Candidates)
	}
	if len(preview.Warnings) == 0 && preview.Error == "" {
		t.Error("预览没有报告这个模板无法组卷")
	}
	// The whole point is catching this before an exam rather than during one.
	if preview.Error == "" {
		t.Error("预览应当给出生成失败的具体原因")
	}
}

func TestExamRejectsPerStudentWithoutTemplate(t *testing.T) {
	h := newHarness(t)
	h.seedBank(t)
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "缺模板", "type": "exam", "per_student_paper": true,
		"items": []map[string]any{{"problem_id": 1, "score": 100}},
	}, true)
	if status != http.StatusBadRequest {
		t.Errorf("status = %d (%s), want 400", status, raw)
	}
}

// writeSnapshotOnly logs a student in and leaves a timed snapshot but no final.
func (h *harness) writeSnapshotOnly(t *testing.T, examID, problemID int64, no, name, code string) int64 {
	t.Helper()
	login := h.login(examID, no, name, "")
	status, raw := h.do(http.MethodPost, "/api/client/snapshot", protocol.SnapshotRequest{
		Items: []protocol.SnapshotItem{{
			ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
		}},
	}, false)
	h.mustJSON(status, raw, nil)
	return login.SessionID
}

func TestReconcileFindsUnsubmittedWork(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()

	// 张三 submits properly.
	code := "int main(){}"
	h.login(examID, "2023001", "张三", "")
	status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
	}, false)
	h.mustJSON(status, raw, nil)

	// 李四 writes code but never presses submit.
	h.writeSnapshotOnly(t, examID, problemID, "2023002", "李四", "int main(){ /* 没点提交 */ }")

	// 王五 logs in and does nothing at all.
	h.login(examID, "2023003", "王五", "")

	var report store.ReconcileReport
	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(examID)+"/reconcile", nil, true)
	h.mustJSON(status, raw, &report)

	byStudent := map[string]store.ReconcileRow{}
	for _, row := range report.Rows {
		byStudent[row.StudentNo] = row
	}
	if got := byStudent["2023001"].Status; got != store.ReconcileComplete {
		t.Errorf("张三 status = %s, want complete", got)
	}
	if got := byStudent["2023002"].Status; got != store.ReconcileRecoverable {
		t.Errorf("李四 status = %s, want recoverable", got)
	}
	if got := byStudent["2023003"].Status; got != store.ReconcileNoWork {
		t.Errorf("王五 status = %s, want no_work", got)
	}

	// Actionable rows come first so the teacher sees them without scrolling.
	if report.Rows[0].Status != store.ReconcileRecoverable {
		t.Errorf("首行 status = %s，可补交的学生应排在最前", report.Rows[0].Status)
	}
	if report.Summary[store.ReconcileRecoverable] != 1 {
		t.Errorf("summary = %v", report.Summary)
	}
}

func TestPromoteSnapshotRepairsAMissedSubmission(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()
	const code = "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b<<std::endl;}"
	sessionID := h.writeSnapshotOnly(t, examID, problemID, "2023002", "李四", code)

	status, raw := h.do(http.MethodPost,
		"/api/teacher/sessions/"+itoa(sessionID)+"/promote",
		map[string]any{"problem_id": problemID}, true)
	h.mustJSON(status, raw, nil)

	ctx := context.Background()
	rows, err := h.st.ListSubmissionRows(ctx, examID, protocol.KindFinal, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d finals, want 1", len(rows))
	}
	if !rows[0].Promoted {
		t.Error("补交的提交没有被标记来源，成绩单上无法与正常提交区分")
	}

	sub, err := h.st.GetSubmission(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if sub.PromotedFrom == nil {
		t.Error("promoted_from 未记录")
	}
	got, err := h.st.ReadSubmissionCode(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got != code {
		t.Errorf("补交的代码与快照不一致")
	}

	// Repeating it must be refused rather than stacking duplicate finals.
	status, raw = h.do(http.MethodPost,
		"/api/teacher/sessions/"+itoa(sessionID)+"/promote",
		map[string]any{"problem_id": problemID}, true)
	if status != http.StatusConflict {
		t.Errorf("二次补交 status = %d (%s)，应为 409", status, raw)
	}

	// After the repair the student is no longer flagged.
	var report store.ReconcileReport
	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(examID)+"/reconcile", nil, true)
	h.mustJSON(status, raw, &report)
	for _, row := range report.Rows {
		if row.StudentNo == "2023002" && row.Status == store.ReconcileRecoverable {
			t.Error("补交之后该学生仍被标记为待补交")
		}
	}
}

func TestPromoteRefusesWithoutASnapshot(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()
	login := h.login(examID, "2023003", "王五", "")

	status, raw := h.do(http.MethodPost,
		"/api/teacher/sessions/"+itoa(login.SessionID)+"/promote",
		map[string]any{"problem_id": problemID}, true)
	if status != http.StatusConflict {
		t.Errorf("status = %d (%s)，没有快照时应当拒绝", status, raw)
	}
	if !strings.Contains(string(raw), "没有任何代码快照") {
		t.Errorf("拒绝理由不明确: %s", raw)
	}
}

func TestExportCodeZip(t *testing.T) {
	h := newHarness(t)
	problemID, examID := h.seedPractice()

	const code = "int main(){ return 0; }"
	h.login(examID, "2023001", "张三", "")
	status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
	}, false)
	h.mustJSON(status, raw, nil)

	req, _ := http.NewRequest(http.MethodGet,
		h.ts.URL+"/api/teacher/exams/"+itoa(examID)+"/code.zip", nil)
	req.Header.Set("X-Admin-Token", "test-admin-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("产出的不是合法 zip: %v", err)
	}
	names := []string{}
	var codeEntry string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.HasSuffix(f.Name, ".cpp") {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			codeEntry = string(b)
		}
	}

	joined := strings.Join(names, "\n")
	if !strings.Contains(joined, "成绩汇总.csv") {
		t.Errorf("压缩包缺少成绩汇总: %v", names)
	}
	if !strings.Contains(joined, "2023001-张三") {
		t.Errorf("代码未按学号-姓名归档: %v", names)
	}
	if !strings.Contains(codeEntry, code) {
		t.Errorf("导出的代码内容不对: %q", codeEntry)
	}
	// A marking teacher needs to know whose file they are looking at.
	if !strings.Contains(codeEntry, "张三") {
		t.Errorf("导出的代码没有标注学生信息: %q", codeEntry)
	}
}

func TestTemplateCRUD(t *testing.T) {
	h := newHarness(t)
	h.seedBank(t)

	tpl := h.saveTemplate(t, map[string]any{
		"name": "原名",
		"items": []map[string]any{
			{"pick_type": "pool", "pool": []int64{1, 2}, "score": 60},
		},
	})

	tplJSON, _ := json.Marshal(map[string]any{
		"id": tpl.ID, "name": "改名",
		"items": []map[string]any{
			{"pick_type": "pool", "pool": []int64{1, 2}, "score": 30},
			{"pick_type": "fixed", "fixed_problem_id": 3, "score": 70},
		},
	})
	var updated store.PaperTemplate
	status, raw := h.do(http.MethodPut, "/api/teacher/templates/"+itoa(tpl.ID),
		json.RawMessage(tplJSON), true)
	h.mustJSON(status, raw, &updated)
	if updated.Name != "改名" || len(updated.Items) != 2 || updated.TotalScore != 100 {
		t.Errorf("更新结果不对: %+v", updated)
	}

	var list []store.PaperTemplate
	status, raw = h.do(http.MethodGet, "/api/teacher/templates", nil, true)
	h.mustJSON(status, raw, &list)
	if len(list) != 1 {
		t.Errorf("模板列表有 %d 项，应为 1（更新不应新建）", len(list))
	}

	status, raw = h.do(http.MethodDelete, "/api/teacher/templates/"+itoa(tpl.ID), nil, true)
	h.mustJSON(status, raw, nil)
	status, raw = h.do(http.MethodGet, "/api/teacher/templates", nil, true)
	h.mustJSON(status, raw, &list)
	if len(list) != 0 {
		t.Errorf("删除后仍有 %d 个模板", len(list))
	}
}
