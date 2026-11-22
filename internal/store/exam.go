package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"coderoom/internal/protocol"
)

type Exam struct {
	ID              int64           `json:"id"`
	Name            string          `json:"name"`
	Type            string          `json:"type"`
	PaperTemplateID *int64          `json:"paper_template_id"`
	PerStudentPaper bool            `json:"per_student_paper"`
	StartAt         *int64          `json:"start_at"`
	EndAt           *int64          `json:"end_at"`
	DurationMin     int             `json:"duration_min"`
	RosterRequired  bool            `json:"roster_required"`
	Passcode        string          `json:"passcode"`
	ShowResults     bool            `json:"show_results"`
	AllowRejudge    bool            `json:"allow_rejudge"`
	Policy          protocol.Policy `json:"policy"`
	PolicyVersion   int             `json:"policy_version"`
	State           string          `json:"state"`
	CreatedAt       int64           `json:"created_at"`
	UpdatedAt       int64           `json:"updated_at"`
}

// IsOpen reports whether a client may log in to this exam at all.
func (e *Exam) IsOpen() bool {
	return e.State == protocol.ExamStatePublished || e.State == protocol.ExamStateRunning
}

// PaperUnlocked decides whether problem statements may be sent to the client.
// Practice sessions are open as soon as they are published; exams withhold the
// statements until the teacher starts them and the start time has passed, so
// that a client cannot cache the paper ahead of time.
func (e *Exam) PaperUnlocked(nowMS int64) (bool, string) {
	if e.Type == protocol.ExamTypePractice {
		if e.IsOpen() {
			return true, ""
		}
		return false, "练习未开放"
	}
	switch e.State {
	case protocol.ExamStateDraft:
		return false, "考试未发布"
	case protocol.ExamStatePublished:
		return false, "考试尚未开始，请等待教师放行"
	case protocol.ExamStateRunning:
		if e.StartAt != nil && nowMS < *e.StartAt {
			return false, "考试尚未到开始时间"
		}
		return true, ""
	default:
		return false, "考试已结束"
	}
}

// Deadline is the server-authoritative moment submissions stop being accepted.
// end_at wins if set; otherwise it is derived from start_at + duration.
func (e *Exam) Deadline() *int64 {
	if e.EndAt != nil {
		return e.EndAt
	}
	if e.StartAt != nil && e.DurationMin > 0 {
		d := *e.StartAt + int64(e.DurationMin)*60_000
		return &d
	}
	return nil
}

// AcceptsSubmissions is the single gate every write path consults, so an
// expired or ended exam cannot be written to even by a tampered client.
func (e *Exam) AcceptsSubmissions(nowMS int64) (bool, string) {
	if ok, reason := e.PaperUnlocked(nowMS); !ok {
		return false, reason
	}
	if d := e.Deadline(); d != nil && nowMS > *d {
		return false, "已超过交卷时间"
	}
	return true, ""
}

func (e *Exam) ToBrief() protocol.ExamBrief {
	return protocol.ExamBrief{
		ID:               e.ID,
		Name:             e.Name,
		Type:             e.Type,
		State:            e.State,
		RosterRequired:   e.RosterRequired,
		PasscodeRequired: e.Passcode != "",
		StartAt:          e.StartAt,
		EndAt:            e.EndAt,
		DurationMin:      e.DurationMin,
		AllowRejudge:     e.AllowRejudge,
		ShowResults:      e.ShowResults,
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (e *Exam) normalize() {
	if e.Type != protocol.ExamTypeExam {
		e.Type = protocol.ExamTypePractice
	}
	if e.State == "" {
		e.State = protocol.ExamStateDraft
	}
	if e.Policy.Version == 0 {
		e.Policy = protocol.DefaultPolicy()
	}
	if e.PolicyVersion <= 0 {
		e.PolicyVersion = 1
	}
	e.Policy.Version = e.PolicyVersion
	// An exam must never leak results before the teacher releases them.
	if e.Type == protocol.ExamTypeExam {
		e.ShowResults = false
	}
}

// PaperItemInput is one line of a paper as composed in the teacher UI.
type PaperItemInput struct {
	ProblemID int64 `json:"problem_id"`
	Score     int   `json:"score"`
}

// CreateExam inserts the exam and materialises its paper where it can.
//
// Three shapes are supported, all landing in the same papers/paper_items
// tables:
//
//   - an explicit problem list, which becomes one class-wide paper;
//   - a template with per_student_paper off, generated once here so the
//     teacher can review the actual paper before the exam;
//   - a template with per_student_paper on, where nothing is generated now -
//     each student's paper is drawn on their first login.
func (s *Store) CreateExam(ctx context.Context, e *Exam, items []PaperItemInput) error {
	e.normalize()
	policy, err := json.Marshal(e.Policy)
	if err != nil {
		return err
	}
	now := NowMS()

	err = s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO exams(name, type, paper_template_id, per_student_paper,
			  start_at, end_at, duration_min, roster_required, passcode,
			  show_results, allow_rejudge, policy_json, policy_version, state,
			  created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			e.Name, e.Type, e.PaperTemplateID, b2i(e.PerStudentPaper),
			e.StartAt, e.EndAt, e.DurationMin, b2i(e.RosterRequired), e.Passcode,
			b2i(e.ShowResults), b2i(e.AllowRejudge), string(policy), e.PolicyVersion,
			e.State, now, now)
		if err != nil {
			return err
		}
		if e.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		e.CreatedAt, e.UpdatedAt = now, now
		if len(items) == 0 {
			// Template-driven: generated here for a class paper, or lazily at
			// login for per-student papers.
			return nil
		}
		return insertPaperTx(ctx, tx, e.ID, nil, "class", "", items)
	})
	if err != nil {
		return err
	}

	if len(items) == 0 && e.PaperTemplateID != nil && !e.PerStudentPaper {
		if _, err := s.EnsurePaperForStudent(ctx, e, ""); err != nil {
			return fmt.Errorf("按模板生成班级试卷失败: %w", err)
		}
	}
	return nil
}

// EnsurePaperForStudent returns the student's paper, drawing one from the
// exam's template if they do not have it yet.
//
// Generating on first login (rather than up front from a roster) is what lets
// an exam run in free-entry mode and still accept a student who arrives late:
// nobody needs to exist in a list beforehand. The draw is deterministic in
// (template nonce, exam, student), so a client restart hands back the same
// paper rather than reshuffling the questions mid-exam.
func (s *Store) EnsurePaperForStudent(ctx context.Context, e *Exam, studentNo string) (*Paper, error) {
	if p, err := s.PaperForStudent(ctx, e.ID, studentNo); err == nil {
		return p, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if e.PaperTemplateID == nil {
		return nil, ErrNotFound // explicit paper expected, but none was created
	}

	scope, owner := "class", ""
	if e.PerStudentPaper {
		scope, owner = "student", studentNo
	}
	gen, err := s.GeneratePaperFor(ctx, *e.PaperTemplateID, e.ID, owner)
	if err != nil {
		return nil, err
	}

	err = s.write(ctx, func(tx *sql.Tx) error {
		// Two students logging in at the same moment would otherwise both
		// generate the class paper. Re-check inside the write transaction.
		var existing int64
		row := tx.QueryRowContext(ctx, `
			SELECT id FROM papers
			WHERE exam_id=? AND scope=? AND student_no=? LIMIT 1`, e.ID, scope, owner)
		switch err := row.Scan(&existing); {
		case err == nil:
			return nil // someone else won the race; their paper stands
		case errors.Is(err, sql.ErrNoRows):
		default:
			return err
		}
		return insertPaperTx(ctx, tx, e.ID, e.PaperTemplateID, scope, owner, gen.Items)
	})
	if err != nil {
		return nil, err
	}
	return s.PaperForStudent(ctx, e.ID, studentNo)
}

// insertPaperTx materialises one paper with its items.
func insertPaperTx(ctx context.Context, tx *sql.Tx, examID int64, templateID *int64,
	scope, studentNo string, items []PaperItemInput) error {

	res, err := tx.ExecContext(ctx, `
		INSERT INTO papers(exam_id, template_id, scope, student_no, generated_at)
		VALUES(?,?,?,?,?)`, examID, templateID, scope, studentNo, NowMS())
	if err != nil {
		return err
	}
	paperID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for i, it := range items {
		score := it.Score
		if score <= 0 {
			score = 10
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO paper_items(paper_id, idx, problem_id, score) VALUES(?,?,?,?)`,
			paperID, i+1, it.ProblemID, score)
		if err != nil {
			return err
		}
	}
	return nil
}

const examCols = `id, name, type, paper_template_id, per_student_paper, start_at, end_at,
	duration_min, roster_required, passcode, show_results, allow_rejudge,
	policy_json, policy_version, state, created_at, updated_at`

func scanExam(sc interface{ Scan(...any) error }) (*Exam, error) {
	var e Exam
	var perStudent, roster, show, rejudge int
	var policyJSON string
	err := sc.Scan(&e.ID, &e.Name, &e.Type, &e.PaperTemplateID, &perStudent,
		&e.StartAt, &e.EndAt, &e.DurationMin, &roster, &e.Passcode, &show,
		&rejudge, &policyJSON, &e.PolicyVersion, &e.State, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	e.PerStudentPaper = perStudent != 0
	e.RosterRequired = roster != 0
	e.ShowResults = show != 0
	e.AllowRejudge = rejudge != 0
	if policyJSON != "" && policyJSON != "{}" {
		_ = json.Unmarshal([]byte(policyJSON), &e.Policy)
	}
	if e.Policy.Version == 0 {
		e.Policy = protocol.DefaultPolicy()
	}
	e.Policy.Version = e.PolicyVersion
	return &e, nil
}

func (s *Store) GetExam(ctx context.Context, id int64) (*Exam, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+examCols+` FROM exams WHERE id=?`, id)
	e, err := scanExam(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

func (s *Store) ListExams(ctx context.Context) ([]*Exam, error) {
	return s.queryExams(ctx, `SELECT `+examCols+` FROM exams ORDER BY id DESC`)
}

// ListOpenExams is what an unauthenticated client sees on its login screen.
func (s *Store) ListOpenExams(ctx context.Context) ([]*Exam, error) {
	return s.queryExams(ctx, `SELECT `+examCols+` FROM exams
		WHERE state IN ('published','running') ORDER BY id DESC`)
}

func (s *Store) queryExams(ctx context.Context, query string, args ...any) ([]*Exam, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Exam{}
	for rows.Next() {
		e, err := scanExam(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetExamState advances the lifecycle. start_at is stamped on the transition
// into running when the teacher did not schedule one, so the countdown always
// has a server-side origin.
func (s *Store) SetExamState(ctx context.Context, id int64, state string) error {
	now := NowMS()
	return s.write(ctx, func(tx *sql.Tx) error {
		if state == protocol.ExamStateRunning {
			if _, err := tx.ExecContext(ctx,
				`UPDATE exams SET start_at=COALESCE(start_at, ?) WHERE id=?`, now, id); err != nil {
				return err
			}
		}
		if state == protocol.ExamStateEnded {
			if _, err := tx.ExecContext(ctx,
				`UPDATE exams SET end_at=COALESCE(end_at, ?) WHERE id=?`, now, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE exams SET state=?, updated_at=? WHERE id=?`, state, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetShowResults releases (or hides) judge results for an exam.
func (s *Store) SetShowResults(ctx context.Context, id int64, show bool) error {
	_, err := s.exec(ctx, `UPDATE exams SET show_results=?, updated_at=? WHERE id=?`,
		b2i(show), NowMS(), id)
	return err
}

func (s *Store) DeleteExam(ctx context.Context, id int64) error {
	_, err := s.exec(ctx, `DELETE FROM exams WHERE id=?`, id)
	return err
}

type Paper struct {
	ID        int64       `json:"id"`
	ExamID    int64       `json:"exam_id"`
	Scope     string      `json:"scope"`
	StudentNo string      `json:"student_no"`
	Items     []PaperItem `json:"items"`
}

type PaperItem struct {
	ID        int64 `json:"id"`
	Idx       int   `json:"idx"`
	ProblemID int64 `json:"problem_id"`
	Score     int   `json:"score"`
}

// PaperForStudent resolves which paper a student sits. A per-student paper
// wins when one exists, otherwise the class-wide paper. M0 only ever creates
// the latter; M2 fills in the former without changing this lookup.
func (s *Store) PaperForStudent(ctx context.Context, examID int64, studentNo string) (*Paper, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, exam_id, scope, student_no FROM papers
		WHERE exam_id=? AND ((scope='student' AND student_no=?) OR scope='class')
		ORDER BY CASE scope WHEN 'student' THEN 0 ELSE 1 END
		LIMIT 1`, examID, studentNo)

	var p Paper
	if err := row.Scan(&p.ID, &p.ExamID, &p.Scope, &p.StudentNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	items, err := s.listPaperItems(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	p.Items = items
	return &p, nil
}

func (s *Store) listPaperItems(ctx context.Context, paperID int64) ([]PaperItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, idx, problem_id, score FROM paper_items WHERE paper_id=? ORDER BY idx`,
		paperID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PaperItem{}
	for rows.Next() {
		var it PaperItem
		if err := rows.Scan(&it.ID, &it.Idx, &it.ProblemID, &it.Score); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ReplaceClassPaper rewrites the class-wide paper for an exam.
func (s *Store) ReplaceClassPaper(ctx context.Context, examID int64, items []PaperItemInput) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM papers WHERE exam_id=? AND scope='class'`, examID); err != nil {
			return err
		}
		return insertPaperTx(ctx, tx, examID, nil, "class", "", items)
	})
}
