package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

const (
	maxCodeBytes     = 512 << 10 // 512 KiB: far beyond any exam answer
	maxStudentNoLen  = 40
	maxNameLen       = 40
	clockSkewAlertMS = 120_000 // 2 minutes
)

// handleClientExams is the only unauthenticated endpoint that reveals
// anything, and it reveals only what the login screen needs: exam names and
// whether a passcode is required. No problem data.
func (s *Server) handleClientExams(w http.ResponseWriter, r *http.Request) {
	exams, err := s.st.ListOpenExams(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	resp := protocol.ExamListResponse{Envelope: s.envelope(0), Exams: []protocol.ExamBrief{}}
	for _, e := range exams {
		resp.Exams = append(resp.Exams, e.ToBrief())
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleClientLogin(w http.ResponseWriter, r *http.Request) {
	var req protocol.LoginRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	req.StudentNo = strings.TrimSpace(req.StudentNo)
	req.Name = strings.TrimSpace(req.Name)

	switch {
	case req.StudentNo == "" || req.Name == "":
		writeError(w, http.StatusBadRequest, "missing_identity", "请填写学号和姓名")
		return
	case len(req.StudentNo) > maxStudentNoLen || len(req.Name) > maxNameLen:
		writeError(w, http.StatusBadRequest, "bad_identity", "学号或姓名过长")
		return
	}

	exam, err := s.st.GetExam(r.Context(), req.ExamID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no_exam", "考试不存在")
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if !exam.IsOpen() {
		writeError(w, http.StatusForbidden, "exam_closed", "该考试/练习当前不开放")
		return
	}
	if exam.Passcode != "" && req.Passcode != exam.Passcode {
		writeError(w, http.StatusForbidden, "bad_passcode", "考试口令错误")
		return
	}

	// Roster check. Looking the student up even when the roster is optional
	// lets us fill in their class for the gradebook.
	class := ""
	st, serr := s.st.FindStudent(r.Context(), req.StudentNo)
	switch {
	case serr == nil:
		class = st.Class
		if exam.RosterRequired && st.Name != req.Name {
			writeError(w, http.StatusForbidden, "name_mismatch", "姓名与名单不符，请核对后重新输入")
			return
		}
	case errors.Is(serr, store.ErrNotFound):
		if exam.RosterRequired {
			writeError(w, http.StatusForbidden, "not_in_roster", "学号不在考试名单中，请联系教师")
			return
		}
	default:
		writeError(w, http.StatusInternalServerError, "db_error", serr.Error())
		return
	}

	paper, err := s.st.EnsurePaperForStudent(r.Context(), exam, req.StudentNo)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, "no_paper", "教师尚未为该考试配置试卷")
		return
	} else if err != nil {
		// A generation failure here means the template cannot be satisfied
		// (a filter matching nothing, say). Say so rather than returning a
		// bare database error, because only the teacher can fix it.
		s.logger.Printf("为 %s 生成试卷失败: %v", req.StudentNo, err)
		writeError(w, http.StatusConflict, "paper_generation_failed",
			"无法生成试卷，请联系教师检查组卷模板: "+err.Error())
		return
	}

	sess := &store.Session{
		ExamID:        exam.ID,
		StudentNo:     req.StudentNo,
		Name:          req.Name,
		Class:         class,
		MachineName:   req.Machine.Hostname,
		IP:            clientIP(r),
		MAC:           req.Machine.MAC,
		OS:            req.Machine.OS,
		ClientVersion: req.Machine.ClientVersion,
		PaperID:       &paper.ID,
	}
	res, err := s.st.Login(r.Context(), sess)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	// Same student number, different machine: not blocked (a genuine machine
	// swap mid-exam must not lock a student out) but always reported.
	if res.PrevMachine != "" {
		s.logEvent(r, sess, protocol.Event{
			Type:     protocol.EvLoginConflict,
			Severity: protocol.SevAlert,
			Payload: map[string]any{
				"previous_machine": res.PrevMachine,
				"current_machine":  sess.MachineName,
			},
		})
	}
	s.logEvent(r, sess, protocol.Event{
		Type:     protocol.EvClientStarted,
		Severity: protocol.SevInfo,
		Payload:  map[string]any{"machine": sess.MachineName, "ip": sess.IP},
	})
	s.checkClockSkew(r, sess, req.ClientTime)

	writeJSON(w, http.StatusOK, protocol.LoginResponse{
		Envelope:  s.envelope(exam.PolicyVersion),
		Token:     sess.Token,
		SessionID: sess.ID,
		StudentNo: sess.StudentNo,
		Name:      sess.Name,
		Class:     sess.Class,
		Exam:      exam.ToBrief(),
		Policy:    exam.Policy,
	})
}

// checkClockSkew records a client whose clock disagrees with the server. The
// countdown always uses server time, so skew is not a correctness problem -
// but a deliberately altered clock is worth knowing about.
//
// This runs on every heartbeat, which is six times a minute, so what gets
// reported is a *change*: the first time a machine is found out of step, and
// again only if its offset later moves by more than the threshold. A clock
// that is simply set wrong produces one event for the whole exam instead of
// seven hundred identical ones, and an actual mid-exam clock change still
// produces its own.
func (s *Server) checkClockSkew(r *http.Request, sess *store.Session, clientTime int64) {
	if clientTime == 0 {
		return
	}
	skew := store.NowMS() - clientTime
	if skew < 0 {
		skew = -skew
	}
	if skew <= clockSkewAlertMS {
		return
	}
	prev, _ := s.skewSeen.Load(sess.ID)
	if last, ok := prev.(int64); ok && abs64(skew-last) <= clockSkewAlertMS {
		return
	}
	s.skewSeen.Store(sess.ID, skew)
	s.logEvent(r, sess, protocol.Event{
		Type:       protocol.EvClockSkew,
		Severity:   protocol.SevWarn,
		ClientTime: clientTime,
		Payload:    map[string]any{"skew_ms": skew},
	})
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (s *Server) logEvent(r *http.Request, sess *store.Session, ev protocol.Event) {
	s.logEventCtx(r.Context(), sess, ev)
}

// logEventCtx is the request-free form, for events the server raises on its
// own initiative rather than in response to a client.
func (s *Server) logEventCtx(ctx context.Context, sess *store.Session, ev protocol.Event) {
	sid, eid := sess.ID, sess.ExamID
	if err := s.st.LogEvent(ctx, &sid, &eid, sess.StudentNo, ev); err != nil {
		s.logger.Printf("log event %s: %v", ev.Type, err)
	}
}

func (s *Server) handleClientPaper(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	exam, err := s.st.GetExam(r.Context(), sess.ExamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	paper, err := s.st.EnsurePaperForStudent(r.Context(), exam, sess.StudentNo)
	if err != nil {
		writeError(w, http.StatusConflict, "no_paper", "试卷未配置")
		return
	}

	resp := protocol.PaperResponse{
		Envelope: s.envelope(exam.PolicyVersion),
		PaperID:  paper.ID,
		Deadline: exam.Deadline(),
		Items:    []protocol.PaperItem{},
		Drafts:   []protocol.Draft{},
	}
	unlocked, reason := exam.PaperUnlocked(store.NowMS())
	resp.Locked, resp.Reason = !unlocked, reason

	for _, it := range paper.Items {
		pi := protocol.PaperItem{Index: it.Idx, ProblemID: it.ProblemID, Score: it.Score}
		// While locked, the client learns how many problems there are and what
		// each is worth, but not a word of the statements.
		if unlocked {
			p, err := s.st.GetProblem(r.Context(), it.ProblemID)
			if err == nil {
				pi.Problem = p.ToProtocol()
			}
		}
		resp.Items = append(resp.Items, pi)
	}

	if unlocked {
		drafts, err := s.st.LatestDrafts(r.Context(), sess.ID)
		if err != nil {
			s.logger.Printf("load drafts for session %d: %v", sess.ID, err)
		} else {
			resp.Drafts = drafts
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// paperProblemSet is the authorisation check for every write: a client may
// only submit against problems that are actually on its own paper.
func (s *Server) paperProblemSet(r *http.Request, sess *store.Session) (map[int64]*store.PaperItem, error) {
	paper, err := s.st.PaperForStudent(r.Context(), sess.ExamID, sess.StudentNo)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]*store.PaperItem, len(paper.Items))
	for i := range paper.Items {
		set[paper.Items[i].ProblemID] = &paper.Items[i]
	}
	return set, nil
}

func (s *Server) handleClientSnapshot(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	var req protocol.SnapshotRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	exam, err := s.st.GetExam(r.Context(), sess.ExamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if ok, reason := exam.AcceptsSubmissions(store.NowMS()); !ok {
		writeError(w, http.StatusForbidden, "closed", reason)
		return
	}
	items, err := s.paperProblemSet(r, sess)
	if err != nil {
		writeError(w, http.StatusConflict, "no_paper", "试卷未配置")
		return
	}

	resp := protocol.SnapshotResponse{
		Envelope: s.envelope(exam.PolicyVersion),
		Stored:   []int64{},
		Skipped:  []int64{},
	}
	for _, it := range req.Items {
		pi, ok := items[it.ProblemID]
		if !ok || len(it.Code) > maxCodeBytes {
			continue // not on this student's paper, or absurdly large
		}
		latest, err := s.st.LatestHash(r.Context(), sess.ID, it.ProblemID)
		if err != nil {
			s.logger.Printf("latest hash: %v", err)
			continue
		}
		if latest != "" && latest == store.HashCode(it.Code) {
			resp.Skipped = append(resp.Skipped, it.ProblemID)
			continue
		}
		sub := &store.Submission{
			SessionID:   sess.ID,
			ExamID:      sess.ExamID,
			StudentNo:   sess.StudentNo,
			ProblemID:   it.ProblemID,
			PaperItemID: &pi.ID,
			Language:    normalizeLang(it.Language),
			Kind:        protocol.KindSnapshot,
		}
		if err := s.st.SaveSubmission(r.Context(), sub, it.Code); err != nil {
			s.logger.Printf("save snapshot: %v", err)
			continue
		}
		resp.Stored = append(resp.Stored, it.ProblemID)
	}
	s.checkClockSkew(r, sess, req.ClientTime)
	_ = s.st.Heartbeat(r.Context(), sess.ID)
	writeJSON(w, http.StatusOK, resp)
}

func normalizeLang(l string) string {
	if _, ok := map[string]bool{
		protocol.LangCPP: true, protocol.LangC: true, protocol.LangPython: true,
	}[l]; ok {
		return l
	}
	return protocol.LangCPP
}

func (s *Server) handleClientSubmit(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	var req protocol.SubmitRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	if len(req.Code) == 0 {
		writeError(w, http.StatusBadRequest, "empty_code", "代码为空，无法提交")
		return
	}
	if len(req.Code) > maxCodeBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "code_too_large", "代码过长")
		return
	}
	// A mismatching hash means the body was altered in transit. Reject rather
	// than silently storing something the student did not write.
	if req.Hash != "" && req.Hash != store.HashCode(req.Code) {
		writeError(w, http.StatusBadRequest, "hash_mismatch", "代码校验失败，请重试提交")
		return
	}

	exam, err := s.st.GetExam(r.Context(), sess.ExamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	if ok, reason := exam.AcceptsSubmissions(store.NowMS()); !ok {
		writeError(w, http.StatusForbidden, "closed", reason)
		return
	}

	items, err := s.paperProblemSet(r, sess)
	if err != nil {
		writeError(w, http.StatusConflict, "no_paper", "试卷未配置")
		return
	}
	pi, ok := items[req.ProblemID]
	if !ok {
		writeError(w, http.StatusForbidden, "not_on_paper", "该题目不在你的试卷中")
		return
	}

	problem, err := s.st.GetProblem(r.Context(), req.ProblemID)
	if err != nil {
		writeError(w, http.StatusNotFound, "no_problem", "题目不存在")
		return
	}
	lang := normalizeLang(req.Language)
	if !contains(problem.Languages, lang) {
		writeError(w, http.StatusBadRequest, "lang_not_allowed", "该题目不允许使用该语言")
		return
	}

	sub := &store.Submission{
		SessionID:   sess.ID,
		ExamID:      sess.ExamID,
		StudentNo:   sess.StudentNo,
		ProblemID:   req.ProblemID,
		PaperItemID: &pi.ID,
		Language:    lang,
		Kind:        protocol.KindFinal,
	}
	if !exam.AllowRejudge {
		n, err := s.st.CountFinals(r.Context(), sess.ID, req.ProblemID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		if n > 0 {
			writeError(w, http.StatusConflict, "already_submitted",
				"该题目已提交，且本场不允许重复提交")
			return
		}
	}
	if err := s.st.SaveSubmission(r.Context(), sub, req.Code); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	// Both practice and exam submissions are queued immediately: judging as
	// they arrive spreads the CPU cost across the session instead of piling
	// 50 students x N problems onto the server the moment the exam ends.
	// Exams simply keep the *results* hidden until the teacher releases them.
	priority := 0
	if exam.Type == protocol.ExamTypePractice {
		priority = 10 // a student is watching, so jump the queue
	}
	if err := s.st.Enqueue(r.Context(), sub.ID, priority); err != nil {
		s.logger.Printf("enqueue submission %d: %v", sub.ID, err)
	}
	s.checkClockSkew(r, sess, req.ClientTime)
	_ = s.st.Heartbeat(r.Context(), sess.ID)

	writeJSON(w, http.StatusOK, protocol.SubmitResponse{
		Envelope:     s.envelope(exam.PolicyVersion),
		SubmissionID: sub.ID,
		Queued:       true,
		Message:      "提交成功",
	})
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Server) handleClientHeartbeat(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	var req protocol.HeartbeatRequest
	if err := readJSON(r, &req); err != nil {
		req = protocol.HeartbeatRequest{}
	}
	exam, err := s.st.GetExam(r.Context(), sess.ExamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	// The heartbeat carries what the student is looking at, not just that the
	// client is alive, so the dashboard has a live view that costs no events.
	if err := s.st.RecordClientState(r.Context(), sess.ID, req.Focused, req.Foreground); err != nil {
		s.logger.Printf("heartbeat: %v", err)
	}
	s.checkClockSkew(r, sess, req.ClientTime)

	resp := protocol.HeartbeatResponse{
		Envelope:  s.envelope(exam.PolicyVersion),
		ExamState: exam.State,
		Deadline:  exam.Deadline(),
		Commands:  []protocol.Command{},
	}
	// The policy rides down on the heartbeat whenever the client is out of
	// date. This is what makes a late-booting or rebooted machine converge on
	// the current policy by itself, instead of depending on a push that it
	// may have missed.
	if req.PolicyVersion != exam.PolicyVersion {
		p := exam.Policy
		resp.Policy = &p
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleClientEvents(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	var req protocol.EventsRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	const maxBatch = 200
	if len(req.Events) > maxBatch {
		req.Events = req.Events[:maxBatch]
	}
	for _, ev := range req.Events {
		if ev.Type == "" {
			continue
		}
		s.logEvent(r, sess, ev)
	}
	_ = s.st.Heartbeat(r.Context(), sess.ID)
	writeJSON(w, http.StatusOK, s.envelope(0))
}

func (s *Server) handleClientHandin(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	if err := s.st.MarkSubmitted(r.Context(), sess.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.envelope(0))
}

func (s *Server) handleClientResults(w http.ResponseWriter, r *http.Request, sess *store.Session) {
	exam, err := s.st.GetExam(r.Context(), sess.ExamID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	resp := protocol.ResultsResponse{
		Envelope: s.envelope(exam.PolicyVersion),
		Results:  []protocol.SubmissionResult{},
	}
	if !exam.ShowResults {
		resp.Visible = false
		resp.Reason = "成绩尚未公布"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	results, err := s.st.ResultsForSession(r.Context(), sess.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	resp.Visible = true
	resp.Results = results
	writeJSON(w, http.StatusOK, resp)
}
