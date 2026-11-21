package server

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"coderoom/internal/judge"
	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

// ------------------------------------------------------------------- system

type systemInfo struct {
	Version       string          `json:"version"`
	Port          int             `json:"port"`
	LocalIPs      []string        `json:"local_ips"`
	DiscoveryPort int             `json:"discovery_port"`
	Workers       int             `json:"workers"`
	JudgeQueue    string          `json:"judge_queue"`
	Toolchain     judge.Toolchain `json:"toolchain"`
	Available     map[string]bool `json:"available"`
	Override      judge.Toolchain `json:"override"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	tc := s.judger.Toolchain()
	writeJSON(w, http.StatusOK, systemInfo{
		Version:       ClientVersion,
		Port:          s.Port(),
		LocalIPs:      LocalIPs(),
		DiscoveryPort: s.cfg.DiscoveryPort,
		Workers:       s.cfg.Workers,
		JudgeQueue:    s.judgeStatus(r.Context()),
		Toolchain:     tc,
		Available:     tc.Available(),
		Override:      s.cfg.Toolchain,
	})
}

// handleSetToolchain lets the teacher pin the judge to specific compilers.
// This exists because a judge whose gcc differs from the lab's IDE produces
// "compiles on my machine, CE on the server" complaints.
func (s *Server) handleSetToolchain(w http.ResponseWriter, r *http.Request) {
	var override judge.Toolchain
	if err := readJSON(r, &override); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	s.cfg.Toolchain = override
	if err := s.saveConfig(); err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	s.judger.SetToolchain(judge.DetectToolchain(s.baseDir, override))
	s.handleSystem(w, r)
}

// ------------------------------------------------------------------ problems

func (s *Server) handleListProblems(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListProblems(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateProblem(w http.ResponseWriter, r *http.Request) {
	var p store.Problem
	if err := readJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if strings.TrimSpace(p.Title) == "" {
		writeError(w, http.StatusBadRequest, "missing_title", "题目标题不能为空")
		return
	}
	if _, err := s.st.CreateProblem(r.Context(), &p); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "problem.create", fmt.Sprint(p.ID), map[string]any{"title": p.Title})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleGetProblem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "题目 ID 无效")
		return
	}
	p, err := s.st.GetProblem(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "题目不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleUpdateProblem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "题目 ID 无效")
		return
	}
	var p store.Problem
	if err := readJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	p.ID = id
	if err := s.st.UpdateProblem(r.Context(), &p); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "题目不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "problem.update", fmt.Sprint(id), map[string]any{"title": p.Title})
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProblem(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "题目 ID 无效")
		return
	}
	if err := s.st.DeleteProblem(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "problem.delete", fmt.Sprint(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// testcaseView carries case contents inline so the teacher UI can edit them
// without a separate file-fetch round trip per case.
type testcaseView struct {
	Idx      int    `json:"idx"`
	Input    string `json:"input"`
	Output   string `json:"output"`
	Score    int    `json:"score"`
	IsSample bool   `json:"is_sample"`
}

func (s *Server) handleGetTestcases(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "题目 ID 无效")
		return
	}
	cases, err := s.st.ListTestcases(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	out := []testcaseView{}
	for _, c := range cases {
		in, _ := os.ReadFile(c.InputPath)
		exp, _ := os.ReadFile(c.OutputPath)
		out = append(out, testcaseView{
			Idx: c.Idx, Input: string(in), Output: string(exp),
			Score: c.Score, IsSample: c.IsSample,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePutTestcases(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "题目 ID 无效")
		return
	}
	var in []store.TestcaseInput
	if err := readJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if err := s.st.ReplaceTestcases(r.Context(), id, in); err != nil {
		writeError(w, http.StatusInternalServerError, "io_error", err.Error())
		return
	}
	s.audit(r, "problem.testcases", fmt.Sprint(id), map[string]any{"count": len(in)})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(in)})
}

// --------------------------------------------------------------------- exams

type examPayload struct {
	store.Exam
	Items []store.PaperItemInput `json:"items"`
}

func (s *Server) handleListExams(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListExams(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateExam(w http.ResponseWriter, r *http.Request) {
	var payload examPayload
	if err := readJSON(r, &payload); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if strings.TrimSpace(payload.Name) == "" {
		writeError(w, http.StatusBadRequest, "missing_name", "名称不能为空")
		return
	}
	if len(payload.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no_items", "请至少选择一道题目")
		return
	}
	e := payload.Exam
	if err := s.st.CreateExam(r.Context(), &e, payload.Items); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.create", fmt.Sprint(e.ID), map[string]any{
		"name": e.Name, "type": e.Type, "items": len(payload.Items),
	})
	writeJSON(w, http.StatusOK, e)
}

// examDetail bundles everything the teacher's exam page needs in one request.
type examDetail struct {
	*store.Exam
	Items []examDetailItem `json:"items"`
}

type examDetailItem struct {
	Idx       int    `json:"idx"`
	ProblemID int64  `json:"problem_id"`
	Title     string `json:"title"`
	Score     int    `json:"score"`
	CaseCount int    `json:"case_count"`
}

func (s *Server) handleGetExam(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	e, err := s.st.GetExam(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "考试不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	detail := examDetail{Exam: e, Items: []examDetailItem{}}
	if paper, err := s.st.PaperForStudent(r.Context(), id, ""); err == nil {
		for _, it := range paper.Items {
			dit := examDetailItem{Idx: it.Idx, ProblemID: it.ProblemID, Score: it.Score}
			if p, err := s.st.GetProblem(r.Context(), it.ProblemID); err == nil {
				dit.Title = p.Title
				dit.CaseCount = p.CaseCount
			}
			detail.Items = append(detail.Items, dit)
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleDeleteExam(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	if err := s.st.DeleteExam(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.delete", fmt.Sprint(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSetExamState(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	var body struct {
		State string `json:"state"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	valid := map[string]bool{
		protocol.ExamStateDraft: true, protocol.ExamStatePublished: true,
		protocol.ExamStateRunning: true, protocol.ExamStateEnded: true,
		protocol.ExamStateJudging: true, protocol.ExamStateGraded: true,
	}
	if !valid[body.State] {
		writeError(w, http.StatusBadRequest, "bad_state", "状态无效")
		return
	}
	if err := s.st.SetExamState(r.Context(), id, body.State); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.state", fmt.Sprint(id), map[string]any{"state": body.State})
	s.handleGetExam(w, r)
}

func (s *Server) handleSetExamPaper(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	var items []store.PaperItemInput
	if err := readJSON(r, &items); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if len(items) == 0 {
		writeError(w, http.StatusBadRequest, "no_items", "请至少选择一道题目")
		return
	}
	if err := s.st.ReplaceClassPaper(r.Context(), id, items); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.paper", fmt.Sprint(id), map[string]any{"items": len(items)})
	s.handleGetExam(w, r)
}

// handleJudgeExam queues the latest final submission of every student+problem.
// Submissions are normally judged as they arrive; this is the teacher's
// explicit "(re)judge everything", needed after fixing test data.
func (s *Server) handleJudgeExam(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	n, err := s.st.EnqueueExamFinals(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if n > 0 {
		_ = s.st.SetExamState(r.Context(), id, protocol.ExamStateJudging)
	}
	s.audit(r, "exam.judge", fmt.Sprint(id), map[string]any{"queued": n})
	writeJSON(w, http.StatusOK, map[string]any{"queued": n})
}

func (s *Server) handleReleaseResults(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	var body struct {
		Show bool `json:"show"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if err := s.st.SetShowResults(r.Context(), id, body.Show); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.release", fmt.Sprint(id), map[string]any{"show": body.Show})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "show": body.Show})
}

// sessionView adds derived invigilation state the dashboard needs.
type sessionView struct {
	*store.Session
	Online      bool  `json:"online"`
	SilentForMS int64 `json:"silent_for_ms"`
}

// A client that has not been heard from for this long is treated as gone -
// the only way to detect a student who killed the client process.
const heartbeatGraceMS = 45_000

func (s *Server) handleExamSessions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	list, err := s.st.ListSessions(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	now := store.NowMS()
	out := []sessionView{}
	for _, x := range list {
		silent := now - x.LastHeartbeatAt
		out = append(out, sessionView{
			Session:     x,
			Online:      silent <= heartbeatGraceMS,
			SilentForMS: silent,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExamSubmissions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	kind := r.URL.Query().Get("kind")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.st.ListSubmissionRows(r.Context(), id, kind, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ------------------------------------------------------------------- scores

type scoreCell struct {
	SubmissionID int64  `json:"submission_id"`
	Verdict      string `json:"verdict"`
	CaseScore    int    `json:"case_score"`
	CaseMax      int    `json:"case_max"`
	PaperScore   int    `json:"paper_score"`
	Scaled       int    `json:"scaled"`
	Language     string `json:"language"`
	TimeMS       int    `json:"time_ms"`
}

type scoreRow struct {
	StudentNo string               `json:"student_no"`
	Name      string               `json:"name"`
	Class     string               `json:"class"`
	Total     int                  `json:"total"`
	Cells     map[string]scoreCell `json:"cells"`
}

type scoresResponse struct {
	ExamID     int64            `json:"exam_id"`
	ExamName   string           `json:"exam_name"`
	TotalScore int              `json:"total_score"`
	Problems   []examDetailItem `json:"problems"`
	Rows       []scoreRow       `json:"rows"`
}

// buildScores assembles the gradebook.
//
// Two score scales meet here. Judge results are in test-case scale (intrinsic
// to the problem, e.g. 5 cases x 10 = 50). The paper assigns a weight per
// problem (e.g. this problem is worth 25 marks in this exam). The cell's
// Scaled value converts the former into the latter, which keeps test data
// independent of how any particular exam weights a problem.
func (s *Server) buildScores(r *http.Request, examID int64) (*scoresResponse, error) {
	exam, err := s.st.GetExam(r.Context(), examID)
	if err != nil {
		return nil, err
	}
	resp := &scoresResponse{
		ExamID:   exam.ID,
		ExamName: exam.Name,
		Problems: []examDetailItem{},
		Rows:     []scoreRow{},
	}

	paperScore := map[int64]int{}
	if paper, err := s.st.PaperForStudent(r.Context(), examID, ""); err == nil {
		for _, it := range paper.Items {
			dit := examDetailItem{Idx: it.Idx, ProblemID: it.ProblemID, Score: it.Score}
			if p, err := s.st.GetProblem(r.Context(), it.ProblemID); err == nil {
				dit.Title = p.Title
				dit.CaseCount = p.CaseCount
			}
			resp.Problems = append(resp.Problems, dit)
			paperScore[it.ProblemID] = it.Score
			resp.TotalScore += it.Score
		}
	}

	// Rows are seeded from sessions so a student who logged in but submitted
	// nothing still appears (with zeros) instead of vanishing.
	sessions, err := s.st.ListSessions(r.Context(), examID)
	if err != nil {
		return nil, err
	}
	byStudent := map[string]*scoreRow{}
	for _, sess := range sessions {
		byStudent[sess.StudentNo] = &scoreRow{
			StudentNo: sess.StudentNo,
			Name:      sess.Name,
			Class:     sess.Class,
			Cells:     map[string]scoreCell{},
		}
	}

	best, err := s.st.ExamScores(r.Context(), examID)
	if err != nil {
		return nil, err
	}
	for _, b := range best {
		row, ok := byStudent[b.StudentNo]
		if !ok {
			row = &scoreRow{
				StudentNo: b.StudentNo, Name: b.Name, Class: b.Class,
				Cells: map[string]scoreCell{},
			}
			byStudent[b.StudentNo] = row
		}
		ps := paperScore[b.ProblemID]
		scaled := 0
		if b.MaxScore > 0 {
			scaled = int(float64(ps)*float64(b.Score)/float64(b.MaxScore) + 0.5)
		}
		row.Cells[fmt.Sprint(b.ProblemID)] = scoreCell{
			SubmissionID: b.SubmissionID,
			Verdict:      b.Verdict,
			CaseScore:    b.Score,
			CaseMax:      b.MaxScore,
			PaperScore:   ps,
			Scaled:       scaled,
			Language:     b.Language,
			TimeMS:       b.TimeMS,
		}
		row.Total += scaled
	}

	for _, row := range byStudent {
		resp.Rows = append(resp.Rows, *row)
	}
	sort.Slice(resp.Rows, func(i, j int) bool {
		if resp.Rows[i].Class != resp.Rows[j].Class {
			return resp.Rows[i].Class < resp.Rows[j].Class
		}
		return resp.Rows[i].StudentNo < resp.Rows[j].StudentNo
	})
	return resp, nil
}

func (s *Server) handleExamScores(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	resp, err := s.buildScores(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleExamScoresCSV(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	resp, err := s.buildScores(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="scores-%d.csv"`, id))
	// Excel on Windows reads a BOM-less UTF-8 CSV as the local ANSI codepage
	// and mangles every Chinese name, so the BOM is required, not cosmetic.
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})

	cw := csv.NewWriter(w)
	defer cw.Flush()

	header := []string{"班级", "学号", "姓名"}
	for _, p := range resp.Problems {
		header = append(header, fmt.Sprintf("第%d题 %s (%d分)", p.Idx, p.Title, p.Score))
	}
	header = append(header, fmt.Sprintf("总分 (%d分)", resp.TotalScore))
	_ = cw.Write(header)

	for _, row := range resp.Rows {
		rec := []string{row.Class, row.StudentNo, row.Name}
		for _, p := range resp.Problems {
			if cell, ok := row.Cells[fmt.Sprint(p.ProblemID)]; ok {
				rec = append(rec, fmt.Sprintf("%d (%s)", cell.Scaled, cell.Verdict))
			} else {
				rec = append(rec, "")
			}
		}
		rec = append(rec, strconv.Itoa(row.Total))
		_ = cw.Write(rec)
	}
}

func (s *Server) handleExamEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := s.st.ListEvents(r.Context(), id, since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// -------------------------------------------------------------- submissions

type submissionDetail struct {
	*store.Submission
	ProblemTitle string                `json:"problem_title"`
	StudentName  string                `json:"student_name"`
	Code         string                `json:"code"`
	Result       *store.JudgeResult    `json:"result"`
	Timeline     []store.SubmissionRow `json:"timeline"`
}

func (s *Server) handleGetSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "提交 ID 无效")
		return
	}
	sub, err := s.st.GetSubmission(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "提交不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	detail := submissionDetail{Submission: sub, Timeline: []store.SubmissionRow{}}
	if code, err := s.st.ReadSubmissionCode(sub); err == nil {
		detail.Code = code
	} else {
		detail.Code = "// 读取代码文件失败: " + err.Error()
	}
	if p, err := s.st.GetProblem(r.Context(), sub.ProblemID); err == nil {
		detail.ProblemTitle = p.Title
	}
	if res, err := s.st.GetJudgeResult(r.Context(), id); err == nil {
		detail.Result = res
	}
	if tl, err := s.st.SnapshotTimeline(r.Context(), sub.SessionID, sub.ProblemID); err == nil {
		detail.Timeline = tl
		if len(tl) > 0 {
			detail.StudentName = tl[0].Name
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleRejudge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "提交 ID 无效")
		return
	}
	if _, err := s.st.GetSubmission(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "提交不存在")
		return
	}
	if err := s.st.Enqueue(r.Context(), id, 20); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "submission.rejudge", fmt.Sprint(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ students

func (s *Server) handleImportStudents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Batch string `json:"batch"`
		CSV   string `json:"csv"`
	}
	if err := readJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	list, warnings := parseRosterCSV(body.CSV)
	if len(list) == 0 {
		writeError(w, http.StatusBadRequest, "empty_roster",
			"未解析到任何学生，请检查格式（每行：学号,姓名,班级）")
		return
	}
	n, err := s.st.ImportStudents(r.Context(), list, body.Batch)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "students.import", body.Batch, map[string]any{"count": n})
	writeJSON(w, http.StatusOK, map[string]any{"imported": n, "warnings": warnings})
}

// parseRosterCSV accepts "学号,姓名,班级" with optional header, tolerating
// commas, tabs and full-width commas so a teacher can paste from Excel.
func parseRosterCSV(text string) ([]store.Student, []string) {
	var out []store.Student
	var warnings []string
	text = strings.TrimPrefix(text, "\ufeff")

	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
		if line == "" {
			continue
		}
		line = strings.NewReplacer("\t", ",", "，", ",", ";", ",").Replace(line)
		parts := strings.Split(line, ",")
		for j := range parts {
			parts[j] = strings.TrimSpace(parts[j])
		}
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			warnings = append(warnings, fmt.Sprintf("第 %d 行格式不足，已跳过: %q", i+1, line))
			continue
		}
		// Skip a header row rather than importing a student called "姓名".
		if i == 0 && (parts[0] == "学号" || strings.EqualFold(parts[0], "student_no")) {
			continue
		}
		st := store.Student{StudentNo: parts[0], Name: parts[1]}
		if len(parts) >= 3 {
			st.Class = parts[2]
		}
		out = append(out, st)
	}
	return out, warnings
}

func (s *Server) handleListStudents(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListStudents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// audit records a teacher action. Best-effort: an audit failure must not break
// the action the teacher asked for.
func (s *Server) audit(r *http.Request, action, target string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["from"] = clientIP(r)
	if err := s.st.LogAudit(r.Context(), "teacher", action, target, payload); err != nil {
		s.logger.Printf("audit %s: %v", action, err)
	}
}
