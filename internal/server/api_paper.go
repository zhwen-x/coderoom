package server

import (
	"archive/zip"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"coderoom/internal/store"
)

// ---------------------------------------------------------------- templates

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListTemplates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleSaveTemplate(w http.ResponseWriter, r *http.Request) {
	var t store.PaperTemplate
	if err := readJSON(r, &t); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if strings.TrimSpace(t.Name) == "" {
		writeError(w, http.StatusBadRequest, "missing_name", "模板名称不能为空")
		return
	}
	if len(t.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no_items", "请至少添加一个题位")
		return
	}
	if id, err := pathID(r, "id"); err == nil {
		t.ID = id
	}
	if err := s.st.SaveTemplate(r.Context(), &t); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "template.save", fmt.Sprint(t.ID),
		map[string]any{"name": t.Name, "items": len(t.Items)})
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "模板 ID 无效")
		return
	}
	t, err := s.st.GetTemplate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "模板不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "模板 ID 无效")
		return
	}
	if err := s.st.DeleteTemplate(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "template.delete", fmt.Sprint(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRerollTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "模板 ID 无效")
		return
	}
	if err := s.st.RerollTemplate(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "template.reroll", fmt.Sprint(id), nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// previewItem reports what one template item can draw from.
type previewItem struct {
	Idx        int      `json:"idx"`
	PickType   string   `json:"pick_type"`
	Score      int      `json:"score"`
	Rule       string   `json:"rule"`
	Candidates int      `json:"candidates"`
	Samples    []string `json:"samples"`
}

type previewSample struct {
	StudentNo string   `json:"student_no"`
	Titles    []string `json:"titles"`
}

type templatePreview struct {
	TemplateID int64           `json:"template_id"`
	Name       string          `json:"name"`
	TotalScore int             `json:"total_score"`
	Items      []previewItem   `json:"items"`
	Samples    []previewSample `json:"samples"`
	Warnings   []string        `json:"warnings"`
	Error      string          `json:"error"`
}

// handlePreviewTemplate validates a template and shows what it would actually
// produce. Running this before an exam is what turns "no problem matches this
// filter" from a mid-exam failure into a configuration message.
func (s *Server) handlePreviewTemplate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "模板 ID 无效")
		return
	}
	t, err := s.st.GetTemplate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "模板不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	candidates, err := s.st.Candidates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	titles, err := s.st.ProblemTitles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	out := templatePreview{
		TemplateID: t.ID, Name: t.Name, TotalScore: t.TotalScore,
		Items: []previewItem{}, Samples: []previewSample{}, Warnings: []string{},
	}

	for _, item := range t.Items {
		pi := previewItem{
			Idx: item.Idx, PickType: item.PickType, Score: item.Score,
			Rule: store.DescribeRule(item), Samples: []string{},
		}
		for _, c := range store.EligibleCandidates(item, candidates) {
			pi.Candidates++
			if len(pi.Samples) < 6 {
				pi.Samples = append(pi.Samples, titles[c.ID])
			}
		}
		if item.PickType != store.PickFixed && pi.Candidates == 0 {
			out.Warnings = append(out.Warnings,
				fmt.Sprintf("第 %d 题没有任何可选题目（%s）", item.Idx, pi.Rule))
		}
		out.Items = append(out.Items, pi)
	}

	// Draw for a handful of stand-in students so the teacher can see both the
	// spread and that generation actually succeeds end to end.
	for _, no := range []string{"示例学号A", "示例学号B", "示例学号C"} {
		gen, err := s.st.GeneratePaperFor(r.Context(), t.ID, 0, no)
		if err != nil {
			out.Error = err.Error()
			break
		}
		sample := previewSample{StudentNo: no, Titles: []string{}}
		for _, it := range gen.Items {
			sample.Titles = append(sample.Titles, titles[it.ProblemID])
		}
		out.Samples = append(out.Samples, sample)
		for _, warn := range gen.Warnings {
			if !contains(out.Warnings, warn) {
				out.Warnings = append(out.Warnings, warn)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// -------------------------------------------------------------- reconcile

func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	report, err := s.st.Reconcile(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handlePromoteSnapshot repairs one missing hand-in. Deliberately one student
// and one problem per call: a bulk button would make it too easy to submit
// drafts a student had abandoned on purpose.
func (s *Server) handlePromoteSnapshot(w http.ResponseWriter, r *http.Request) {
	sessionID, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "会话 ID 无效")
		return
	}
	var body struct {
		ProblemID int64 `json:"problem_id"`
	}
	if err := readJSON(r, &body); err != nil || body.ProblemID == 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "请指定题目")
		return
	}

	sub, err := s.st.PromoteSnapshot(r.Context(), sessionID, body.ProblemID)
	if err != nil {
		writeError(w, http.StatusConflict, "promote_failed", err.Error())
		return
	}
	if err := s.st.Enqueue(r.Context(), sub.ID, 5); err != nil {
		s.logger.Printf("enqueue promoted submission %d: %v", sub.ID, err)
	}
	s.audit(r, "submission.promote", fmt.Sprint(sub.ID), map[string]any{
		"student_no":    sub.StudentNo,
		"problem_id":    sub.ProblemID,
		"promoted_from": sub.PromotedFrom,
		"code_bytes":    sub.CodeBytes,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "submission_id": sub.ID,
		"message": fmt.Sprintf("已用 %s 的最后快照补交并加入判题队列", sub.StudentNo),
	})
}

// --------------------------------------------------------------- code export

// handleExportCode packages every student's final code as a zip, laid out by
// class and student so a teacher can archive or mark it offline.
func (s *Server) handleExportCode(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	exam, err := s.st.GetExam(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "考试不存在")
		return
	}
	scores, err := s.buildScores(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	sessions, err := s.st.ListSessions(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	titles, err := s.st.ProblemTitles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="code-%d.zip"`, id))

	zw := zip.NewWriter(w)
	defer zw.Close()

	// Headline summary at the root of the archive. Written with a BOM for the
	// same reason as the scores export: Excel on Windows otherwise reads a
	// UTF-8 CSV as ANSI and mangles every Chinese name.
	if f, err := zw.Create("成绩汇总.csv"); err == nil {
		_, _ = f.Write([]byte{0xEF, 0xBB, 0xBF})
		cw := csv.NewWriter(f)
		header := []string{"班级", "学号", "姓名"}
		for _, p := range scores.Problems {
			header = append(header, fmt.Sprintf("第%d题 %s (%d分)", p.Idx, p.Title, p.Score))
		}
		header = append(header, fmt.Sprintf("总分 (%d分)", scores.TotalScore))
		_ = cw.Write(header)
		for _, row := range scores.Rows {
			rec := []string{row.Class, row.StudentNo, row.Name}
			for _, p := range scores.Problems {
				if cell, ok := row.Cells[fmt.Sprint(p.ProblemID)]; ok {
					rec = append(rec, fmt.Sprintf("%d (%s)", cell.Scaled, cell.Verdict))
				} else {
					rec = append(rec, "")
				}
			}
			rec = append(rec, strconv.Itoa(row.Total))
			_ = cw.Write(rec)
		}
		cw.Flush()
	}

	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Class != sessions[j].Class {
			return sessions[i].Class < sessions[j].Class
		}
		return sessions[i].StudentNo < sessions[j].StudentNo
	})

	for _, sess := range sessions {
		class := sess.Class
		if class == "" {
			class = "未分班"
		}
		dir := fmt.Sprintf("%s/%s-%s", zipSafe(class), zipSafe(sess.StudentNo), zipSafe(sess.Name))

		// Each student sits their own paper when the exam draws per student,
		// so the problem order has to come from their paper, not the exam's.
		paper, err := s.st.PaperForStudent(r.Context(), id, sess.StudentNo)
		if err != nil {
			continue
		}
		for _, item := range paper.Items {
			sub, err := s.st.LatestFinal(r.Context(), sess.ID, item.ProblemID)
			if err != nil || sub == nil {
				continue
			}
			code, err := s.st.ReadSubmissionCode(sub)
			if err != nil {
				continue
			}
			name := fmt.Sprintf("%s/第%d题-%s.%s", dir, item.Idx,
				zipSafe(titles[item.ProblemID]), store.SourceExt(sub.Language))
			f, err := zw.Create(name)
			if err != nil {
				s.logger.Printf("zip entry %s: %v", name, err)
				continue
			}
			header := fmt.Sprintf(
				"// %s %s  第%d题 %s\n// 提交时间 %s%s\n\n",
				sess.StudentNo, sess.Name, item.Idx, titles[item.ProblemID],
				formatMS(sub.CreatedAt), promotedNote(sub))
			if store.SourceExt(sub.Language) == "py" {
				header = strings.ReplaceAll(header, "//", "#")
			}
			_, _ = f.Write([]byte(header))
			_, _ = f.Write([]byte(code))
		}
	}
	s.audit(r, "exam.export_code", fmt.Sprint(id), map[string]any{"exam": exam.Name})
}

func promotedNote(sub *store.Submission) string {
	if sub.PromotedFrom != nil {
		return "（由教师用定时快照补交）"
	}
	return ""
}

// zipSafe strips characters that Windows refuses in a path. Class names,
// student names and problem titles are all teacher- or student-supplied.
func zipSafe(s string) string {
	if s == "" {
		return "未命名"
	}
	out := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '\n', '\r', '\t':
			return '_'
		}
		return r
	}, s)
	if len([]rune(out)) > 40 {
		out = string([]rune(out)[:40])
	}
	return out
}

// formatMS renders a stored timestamp for a human reading an exported file.
func formatMS(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}
