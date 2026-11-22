package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
)

// Reconciliation statuses, roughly in descending order of how much they need
// the teacher's attention.
const (
	ReconcileRecoverable = "recoverable" // 有快照但没提交，可以补交
	ReconcileAbsent      = "absent"      // 在名单里但从未登录
	ReconcileNoWork      = "no_work"     // 登录了但什么也没写
	ReconcileIncomplete  = "incomplete"  // 交了一部分题
	ReconcileComplete    = "complete"    // 每道题都有终稿
)

// ReconcileProblem is one problem's state for one student.
type ReconcileProblem struct {
	Idx           int    `json:"idx"`
	ProblemID     int64  `json:"problem_id"`
	Title         string `json:"title"`
	HasFinal      bool   `json:"has_final"`
	FinalAt       int64  `json:"final_at"`
	HasSnapshot   bool   `json:"has_snapshot"`
	SnapshotAt    int64  `json:"snapshot_at"`
	SnapshotID    int64  `json:"snapshot_id"`
	SnapshotBytes int    `json:"snapshot_bytes"`
}

// Recoverable reports whether the teacher could repair this one by promoting
// the student's last snapshot.
func (p ReconcileProblem) Recoverable() bool {
	return !p.HasFinal && p.HasSnapshot && p.SnapshotBytes > 0
}

type ReconcileRow struct {
	StudentNo       string             `json:"student_no"`
	Name            string             `json:"name"`
	Class           string             `json:"class"`
	InRoster        bool               `json:"in_roster"`
	SessionID       int64              `json:"session_id"` // 0 = never logged in
	LoginAt         int64              `json:"login_at"`
	LastHeartbeatAt int64              `json:"last_heartbeat_at"`
	SubmittedAt     *int64             `json:"submitted_at"`
	Status          string             `json:"status"`
	Problems        []ReconcileProblem `json:"problems"`
}

type ReconcileReport struct {
	ExamID          int64          `json:"exam_id"`
	ExamName        string         `json:"exam_name"`
	ExpectedClasses []string       `json:"expected_classes"`
	Rows            []ReconcileRow `json:"rows"`
	Summary         map[string]int `json:"summary"`
}

// Reconcile builds the after-the-exam report: who is missing, who wrote code
// but never pressed submit, and who finished.
//
// Absence is judged against the classes that actually turned up, not against
// the whole student table. The roster here is global - one import covers every
// class - so comparing an exam taken by one class against all imported
// students would report every other class as absent. Inferring the expected
// classes from the sessions keeps the report meaningful without inventing a
// per-exam roster.
func (s *Store) Reconcile(ctx context.Context, examID int64) (*ReconcileReport, error) {
	exam, err := s.GetExam(ctx, examID)
	if err != nil {
		return nil, err
	}
	sessions, err := s.ListSessions(ctx, examID)
	if err != nil {
		return nil, err
	}

	report := &ReconcileReport{
		ExamID:          exam.ID,
		ExamName:        exam.Name,
		ExpectedClasses: []string{},
		Rows:            []ReconcileRow{},
		Summary:         map[string]int{},
	}

	classesSeen := map[string]bool{}
	for _, sess := range sessions {
		classesSeen[sess.Class] = true
	}
	for c := range classesSeen {
		report.ExpectedClasses = append(report.ExpectedClasses, c)
	}
	sort.Strings(report.ExpectedClasses)

	titles, err := s.ProblemTitles(ctx)
	if err != nil {
		return nil, err
	}

	attended := map[string]bool{}
	for _, sess := range sessions {
		attended[sess.StudentNo] = true

		row := ReconcileRow{
			StudentNo: sess.StudentNo, Name: sess.Name, Class: sess.Class,
			SessionID: sess.ID, LoginAt: sess.LoginAt,
			LastHeartbeatAt: sess.LastHeartbeatAt, SubmittedAt: sess.SubmittedAt,
			Problems: []ReconcileProblem{},
		}
		if _, err := s.FindStudent(ctx, sess.StudentNo); err == nil {
			row.InRoster = true
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}

		// Each student may sit a different paper when the exam draws per
		// student, so the problem list has to be looked up per row.
		paper, err := s.PaperForStudent(ctx, examID, sess.StudentNo)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if paper != nil {
			for _, it := range paper.Items {
				rp := ReconcileProblem{
					Idx: it.Idx, ProblemID: it.ProblemID, Title: titles[it.ProblemID],
				}
				if err := s.fillSubmissionState(ctx, sess.ID, it.ProblemID, &rp); err != nil {
					return nil, err
				}
				row.Problems = append(row.Problems, rp)
			}
		}
		row.Status = classify(row)
		report.Rows = append(report.Rows, row)
	}

	// Roster students from the classes that sat this exam, who never appeared.
	roster, err := s.ListStudents(ctx)
	if err != nil {
		return nil, err
	}
	for _, st := range roster {
		if attended[st.StudentNo] || !classesSeen[st.Class] {
			continue
		}
		report.Rows = append(report.Rows, ReconcileRow{
			StudentNo: st.StudentNo, Name: st.Name, Class: st.Class,
			InRoster: true, Status: ReconcileAbsent, Problems: []ReconcileProblem{},
		})
	}

	// Rows needing action first, then by class and student number.
	rank := map[string]int{
		ReconcileRecoverable: 0, ReconcileAbsent: 1, ReconcileNoWork: 2,
		ReconcileIncomplete: 3, ReconcileComplete: 4,
	}
	sort.Slice(report.Rows, func(i, j int) bool {
		a, b := report.Rows[i], report.Rows[j]
		if rank[a.Status] != rank[b.Status] {
			return rank[a.Status] < rank[b.Status]
		}
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.StudentNo < b.StudentNo
	})
	for _, row := range report.Rows {
		report.Summary[row.Status]++
	}
	return report, nil
}

func classify(row ReconcileRow) string {
	if row.SessionID == 0 {
		return ReconcileAbsent
	}
	if len(row.Problems) == 0 {
		return ReconcileNoWork
	}
	finals, anyWork, recoverable := 0, false, false
	for _, p := range row.Problems {
		if p.HasFinal {
			finals++
			anyWork = true
		}
		if p.HasSnapshot {
			anyWork = true
		}
		if p.Recoverable() {
			recoverable = true
		}
	}
	switch {
	case recoverable:
		return ReconcileRecoverable // actionable, so it outranks the rest
	case !anyWork:
		return ReconcileNoWork
	case finals == len(row.Problems):
		return ReconcileComplete
	default:
		return ReconcileIncomplete
	}
}

// ProblemTitles maps problem id to title, for labelling reports and exports.
func (s *Store) ProblemTitles(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, title FROM problems`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, rows.Err()
}

func (s *Store) fillSubmissionState(ctx context.Context, sessionID, problemID int64, rp *ReconcileProblem) error {
	err := s.db.QueryRowContext(ctx, `
		SELECT created_at FROM submissions
		WHERE session_id=? AND problem_id=? AND kind='final'
		ORDER BY id DESC LIMIT 1`, sessionID, problemID).Scan(&rp.FinalAt)
	switch {
	case err == nil:
		rp.HasFinal = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}

	err = s.db.QueryRowContext(ctx, `
		SELECT id, created_at, code_bytes FROM submissions
		WHERE session_id=? AND problem_id=? AND kind='snapshot'
		ORDER BY id DESC LIMIT 1`, sessionID, problemID).
		Scan(&rp.SnapshotID, &rp.SnapshotAt, &rp.SnapshotBytes)
	switch {
	case err == nil:
		rp.HasSnapshot = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return err
	}
	return nil
}

// PromoteSnapshot turns a student's last timed snapshot into a real
// submission, for the case where they wrote code but never pressed submit.
//
// This is a repair, not a shortcut: it refuses when a final already exists, it
// records which snapshot the code came from, and the caller writes an audit
// entry. The gradebook shows such submissions as promoted so a mark is never
// silently different from what the student actually handed in.
func (s *Store) PromoteSnapshot(ctx context.Context, sessionID, problemID int64) (*Submission, error) {
	var existing int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions
		WHERE session_id=? AND problem_id=? AND kind='final'`,
		sessionID, problemID).Scan(&existing)
	if err != nil {
		return nil, err
	}
	if existing > 0 {
		return nil, fmt.Errorf("该学生此题已有正式提交，无需补交")
	}

	row := s.db.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions
		 WHERE session_id=? AND problem_id=? AND kind='snapshot'
		 ORDER BY id DESC LIMIT 1`, sessionID, problemID)
	snap, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("该学生此题没有任何代码快照，无法补交")
	}
	if err != nil {
		return nil, err
	}

	code, err := s.ReadSubmissionCode(snap)
	if err != nil {
		return nil, fmt.Errorf("读取快照代码失败: %w", err)
	}
	if len(code) == 0 {
		return nil, fmt.Errorf("该学生此题的快照是空的，无法补交")
	}

	promoted := &Submission{
		SessionID:    snap.SessionID,
		ExamID:       snap.ExamID,
		StudentNo:    snap.StudentNo,
		ProblemID:    snap.ProblemID,
		PaperItemID:  snap.PaperItemID,
		Language:     snap.Language,
		Kind:         "final",
		PromotedFrom: &snap.ID,
	}
	if err := s.SaveSubmission(ctx, promoted, code); err != nil {
		return nil, err
	}
	return promoted, nil
}
