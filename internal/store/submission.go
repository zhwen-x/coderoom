package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coderoom/internal/protocol"
)

// HashCode is the canonical content hash used on both sides of the wire, so
// the server can skip storing an unchanged snapshot and detect tampering.
func HashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// SourceExt maps a language to the extension its source file gets on disk.
func SourceExt(lang string) string {
	switch lang {
	case protocol.LangC:
		return "c"
	case protocol.LangPython:
		return "py"
	default:
		return "cpp"
	}
}

type Submission struct {
	ID          int64  `json:"id"`
	SessionID   int64  `json:"session_id"`
	ExamID      int64  `json:"exam_id"`
	StudentNo   string `json:"student_no"`
	ProblemID   int64  `json:"problem_id"`
	PaperItemID *int64 `json:"paper_item_id"`
	Language    string `json:"language"`
	Kind        string `json:"kind"`
	Seq         int    `json:"seq"`
	CodePath    string `json:"-"`
	CodeHash    string `json:"code_hash"`
	CodeBytes   int    `json:"code_bytes"`
	CreatedAt   int64  `json:"created_at"`
	// PromotedFrom is the snapshot this submission was lifted from when the
	// teacher repaired a missing hand-in. Nil for a normal submission.
	PromotedFrom *int64 `json:"promoted_from"`
}

// SaveSubmission archives the code to disk and records the row. Every snapshot
// is kept rather than overwritten: the sequence of snapshots is the editing
// timeline that M3 relies on for anti-plagiarism evidence.
func (s *Store) SaveSubmission(ctx context.Context, sub *Submission, code string) error {
	dir := s.SubmissionDir(sub.ExamID, sub.StudentNo, sub.ProblemID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sub.CodeHash = HashCode(code)
	sub.CodeBytes = len(code)
	sub.CreatedAt = NowMS()

	return s.write(ctx, func(tx *sql.Tx) error {
		var seq int
		err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(seq),0)+1 FROM submissions
			WHERE session_id=? AND problem_id=? AND kind=?`,
			sub.SessionID, sub.ProblemID, sub.Kind).Scan(&seq)
		if err != nil {
			return err
		}
		sub.Seq = seq
		sub.CodePath = filepath.Join(dir,
			fmt.Sprintf("%s-%03d.%s", sub.Kind, seq, SourceExt(sub.Language)))

		if err := os.WriteFile(sub.CodePath, []byte(code), 0o644); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO submissions(session_id, exam_id, student_no, problem_id,
			  paper_item_id, language, kind, seq, code_path, code_hash, code_bytes,
			  created_at, promoted_from)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			sub.SessionID, sub.ExamID, sub.StudentNo, sub.ProblemID, sub.PaperItemID,
			sub.Language, sub.Kind, sub.Seq, sub.CodePath, sub.CodeHash,
			sub.CodeBytes, sub.CreatedAt, sub.PromotedFrom)
		if err != nil {
			return err
		}
		sub.ID, err = res.LastInsertId()
		return err
	})
}

// LatestHash returns the content hash of the most recent submission of any
// kind for this student+problem, so an unchanged snapshot can be skipped.
func (s *Store) LatestHash(ctx context.Context, sessionID, problemID int64) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, `
		SELECT code_hash FROM submissions
		WHERE session_id=? AND problem_id=? ORDER BY id DESC LIMIT 1`,
		sessionID, problemID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// LatestDrafts returns the newest stored code per problem for a session. This
// is what a restarted client pulls to recover a student's work in progress.
func (s *Store) LatestDrafts(ctx context.Context, sessionID int64) ([]protocol.Draft, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.problem_id, s.language, s.code_path, s.created_at
		FROM submissions s
		JOIN (SELECT problem_id, MAX(id) AS mid FROM submissions
		      WHERE session_id=? GROUP BY problem_id) m
		  ON s.id = m.mid
		ORDER BY s.problem_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []protocol.Draft{}
	for rows.Next() {
		var d protocol.Draft
		var path string
		if err := rows.Scan(&d.ProblemID, &d.Language, &path, &d.UpdatedAt); err != nil {
			return nil, err
		}
		// A missing file must not block login; the student just loses that draft.
		if b, err := os.ReadFile(path); err == nil {
			d.Code = string(b)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) ReadSubmissionCode(sub *Submission) (string, error) {
	b, err := os.ReadFile(sub.CodePath)
	return string(b), err
}

const submissionCols = `id, session_id, exam_id, student_no, problem_id, paper_item_id,
	language, kind, seq, code_path, code_hash, code_bytes, created_at, promoted_from`

func scanSubmission(sc interface{ Scan(...any) error }) (*Submission, error) {
	var x Submission
	err := sc.Scan(&x.ID, &x.SessionID, &x.ExamID, &x.StudentNo, &x.ProblemID,
		&x.PaperItemID, &x.Language, &x.Kind, &x.Seq, &x.CodePath, &x.CodeHash,
		&x.CodeBytes, &x.CreatedAt, &x.PromotedFrom)
	if err != nil {
		return nil, err
	}
	return &x, nil
}

func (s *Store) GetSubmission(ctx context.Context, id int64) (*Submission, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions WHERE id=?`, id)
	x, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return x, err
}

// ListSubmissions returns submissions for an exam, newest first. kind may be
// empty to include both snapshots and finals.
func (s *Store) ListSubmissions(ctx context.Context, examID int64, kind string, limit int) ([]*Submission, error) {
	if limit <= 0 {
		limit = 500
	}
	query := `SELECT ` + submissionCols + ` FROM submissions WHERE exam_id=?`
	args := []any{examID}
	if kind != "" {
		query += ` AND kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Submission{}
	for rows.Next() {
		x, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- judge queue

type JudgeTask struct {
	ID           int64
	SubmissionID int64
	Priority     int
	State        string
	Attempts     int
}

const (
	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskError   = "error"
)

// Enqueue adds (or re-adds) a submission to the judge queue. A resubmitted
// task is reset to pending so "rejudge" and "judge now" are the same path.
func (s *Store) Enqueue(ctx context.Context, submissionID int64, priority int) error {
	_, err := s.exec(ctx, `
		INSERT INTO judge_tasks(submission_id, priority, state, enqueued_at)
		VALUES(?,?,'pending',?)
		ON CONFLICT(submission_id) DO UPDATE SET
		  state='pending', priority=excluded.priority, worker='', error='',
		  attempts=0, enqueued_at=excluded.enqueued_at,
		  started_at=NULL, finished_at=NULL`,
		submissionID, priority, NowMS())
	return err
}

// RetryTask puts a task back on the queue after a judge-side failure, keeping
// the attempt count so the retry budget actually runs out. Enqueue resets that
// count because it means a human asked for a fresh judging run.
func (s *Store) RetryTask(ctx context.Context, taskID int64, lastErr string) error {
	_, err := s.exec(ctx, `
		UPDATE judge_tasks SET state='pending', worker='', error=?,
		  started_at=NULL, finished_at=NULL
		WHERE id=?`, lastErr, taskID)
	return err
}

// EnqueueExamFinals queues the latest final submission per student+problem for
// a whole exam. This is the teacher's "judge everything now" action.
func (s *Store) EnqueueExamFinals(ctx context.Context, examID int64) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT MAX(id) FROM submissions
		WHERE exam_id=? AND kind='final'
		GROUP BY session_id, problem_id`, examID)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	now := NowMS()
	err = s.write(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO judge_tasks(submission_id, priority, state, enqueued_at)
				VALUES(?,0,'pending',?)
				ON CONFLICT(submission_id) DO UPDATE SET
				  state='pending', worker='', error='', attempts=0,
				  enqueued_at=excluded.enqueued_at,
				  started_at=NULL, finished_at=NULL`, id, now)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return len(ids), err
}

// ClaimTask atomically takes the highest-priority pending task.
func (s *Store) ClaimTask(ctx context.Context, worker string) (*JudgeTask, error) {
	var t JudgeTask
	err := s.write(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT id, submission_id, priority, attempts FROM judge_tasks
			WHERE state='pending' ORDER BY priority DESC, id LIMIT 1`)
		if err := row.Scan(&t.ID, &t.SubmissionID, &t.Priority, &t.Attempts); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE judge_tasks SET state='running', worker=?, attempts=attempts+1, started_at=?
			WHERE id=?`, worker, NowMS(), t.ID)
		return err
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // queue empty
	}
	if err != nil {
		return nil, err
	}
	t.State = TaskRunning
	t.Attempts++
	return &t, nil
}

func (s *Store) FinishTask(ctx context.Context, taskID int64, state, errMsg string) error {
	_, err := s.exec(ctx,
		`UPDATE judge_tasks SET state=?, error=?, finished_at=? WHERE id=?`,
		state, errMsg, NowMS(), taskID)
	return err
}

// RequeueStaleTasks resets tasks left 'running' by a server crash. Called at
// startup so an interrupted batch judge resumes instead of stalling.
func (s *Store) RequeueStaleTasks(ctx context.Context) (int, error) {
	res, err := s.exec(ctx, `
		UPDATE judge_tasks SET state='pending', worker='', started_at=NULL
		WHERE state='running'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// PendingCount reports queue depth for the teacher UI.
func (s *Store) PendingCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM judge_tasks WHERE state IN ('pending','running')`).Scan(&n)
	return n, err
}

// -------------------------------------------------------------- judge results

type JudgeResult struct {
	SubmissionID int64                 `json:"submission_id"`
	Verdict      string                `json:"verdict"`
	Score        int                   `json:"score"`
	MaxScore     int                   `json:"max_score"`
	TimeMS       int                   `json:"time_ms"`
	MemoryKB     int                   `json:"memory_kb"`
	CompileLog   string                `json:"compile_log"`
	Cases        []protocol.CaseResult `json:"cases"`
	JudgedAt     int64                 `json:"judged_at"`
}

func (s *Store) SaveJudgeResult(ctx context.Context, r *JudgeResult) error {
	detail, err := json.Marshal(r.Cases)
	if err != nil {
		return err
	}
	r.JudgedAt = NowMS()
	_, err = s.exec(ctx, `
		INSERT INTO judge_results(submission_id, verdict, score, max_score, time_ms,
		  memory_kb, compile_log, detail_json, judged_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(submission_id) DO UPDATE SET
		  verdict=excluded.verdict, score=excluded.score, max_score=excluded.max_score,
		  time_ms=excluded.time_ms, memory_kb=excluded.memory_kb,
		  compile_log=excluded.compile_log, detail_json=excluded.detail_json,
		  judged_at=excluded.judged_at`,
		r.SubmissionID, r.Verdict, r.Score, r.MaxScore, r.TimeMS, r.MemoryKB,
		r.CompileLog, string(detail), r.JudgedAt)
	return err
}

// ResultsForSession returns every judged final submission for one student,
// newest first. Practice sessions poll this for instant feedback.
func (s *Store) ResultsForSession(ctx context.Context, sessionID int64) ([]protocol.SubmissionResult, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.problem_id, s.seq, s.language, s.created_at,
		       r.verdict, r.score, r.max_score, r.time_ms, r.memory_kb,
		       r.compile_log, r.detail_json, r.judged_at
		FROM submissions s
		LEFT JOIN judge_results r ON r.submission_id = s.id
		WHERE s.session_id=? AND s.kind='final'
		ORDER BY s.id DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanResults(rows)
}

func scanResults(rows *sql.Rows) ([]protocol.SubmissionResult, error) {
	out := []protocol.SubmissionResult{}
	for rows.Next() {
		var r protocol.SubmissionResult
		var verdict, compileLog, detail sql.NullString
		var score, maxScore, timeMS, memKB sql.NullInt64
		var judgedAt sql.NullInt64

		err := rows.Scan(&r.SubmissionID, &r.ProblemID, &r.Seq, &r.Language, &r.CreatedAt,
			&verdict, &score, &maxScore, &timeMS, &memKB, &compileLog, &detail, &judgedAt)
		if err != nil {
			return nil, err
		}
		if verdict.Valid {
			r.Verdict = verdict.String
			r.Score = int(score.Int64)
			r.MaxScore = int(maxScore.Int64)
			r.TimeMS = int(timeMS.Int64)
			r.MemoryKB = int(memKB.Int64)
			r.CompileLog = compileLog.String
			if detail.Valid && detail.String != "" {
				_ = json.Unmarshal([]byte(detail.String), &r.Cases)
			}
			if judgedAt.Valid {
				j := judgedAt.Int64
				r.JudgedAt = &j
			}
		} else {
			r.Verdict = protocol.VerdictPending
		}
		if r.Cases == nil {
			r.Cases = []protocol.CaseResult{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ScoreRow is one cell of the teacher's gradebook.
type ScoreRow struct {
	StudentNo    string `json:"student_no"`
	Name         string `json:"name"`
	Class        string `json:"class"`
	ProblemID    int64  `json:"problem_id"`
	SubmissionID int64  `json:"submission_id"`
	Language     string `json:"language"`
	Verdict      string `json:"verdict"`
	Score        int    `json:"score"`
	MaxScore     int    `json:"max_score"`
	TimeMS       int    `json:"time_ms"`
	MemoryKB     int    `json:"memory_kb"`
}

// ExamScores returns the best judged result per student+problem. "Best" means
// highest score, which is the right rule for practice (where resubmission is
// allowed) and harmless for exams (where only one final is judged).
func (s *Store) ExamScores(ctx context.Context, examID int64) ([]ScoreRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sess.student_no, sess.name, sess.class, s.problem_id, s.id, s.language,
		       r.verdict, r.score, r.max_score, r.time_ms, r.memory_kb
		FROM submissions s
		JOIN sessions sess ON sess.id = s.session_id
		JOIN judge_results r ON r.submission_id = s.id
		JOIN (
		  SELECT s2.session_id, s2.problem_id, MAX(r2.score * 100000 - s2.id) AS rank_key
		  FROM submissions s2
		  JOIN judge_results r2 ON r2.submission_id = s2.id
		  WHERE s2.exam_id=? AND s2.kind='final'
		  GROUP BY s2.session_id, s2.problem_id
		) best
		  ON best.session_id = s.session_id AND best.problem_id = s.problem_id
		 AND best.rank_key = r.score * 100000 - s.id
		WHERE s.exam_id=? AND s.kind='final'
		ORDER BY sess.class, sess.student_no, s.problem_id`, examID, examID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ScoreRow{}
	for rows.Next() {
		var x ScoreRow
		if err := rows.Scan(&x.StudentNo, &x.Name, &x.Class, &x.ProblemID,
			&x.SubmissionID, &x.Language, &x.Verdict, &x.Score, &x.MaxScore,
			&x.TimeMS, &x.MemoryKB); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------- events

// LogEvent records one behaviour event. Events are append-only evidence.
func (s *Store) LogEvent(ctx context.Context, sessionID, examID *int64, studentNo string,
	ev protocol.Event) error {

	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return err
	}
	if ev.Severity == "" {
		ev.Severity = protocol.SevInfo
	}
	_, err = s.exec(ctx, `
		INSERT INTO events(session_id, exam_id, student_no, type, severity,
		  payload_json, client_time, server_time)
		VALUES(?,?,?,?,?,?,?,?)`,
		sessionID, examID, studentNo, ev.Type, ev.Severity, string(payload),
		ev.ClientTime, NowMS())
	return err
}

type EventRow struct {
	ID         int64          `json:"id"`
	StudentNo  string         `json:"student_no"`
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	Payload    map[string]any `json:"payload"`
	ClientTime int64          `json:"client_time"`
	ServerTime int64          `json:"server_time"`
}

// EventQuery narrows an event listing.
//
// Ascending exists because the dashboard asks two different questions of the
// same table. Opening the monitor view wants the newest page ("what has
// happened"), so it reads descending and stops at the limit. The live stream
// wants everything after a cursor, oldest first, so that a burst larger than
// one page is delivered in order instead of having its oldest rows silently
// dropped off the end of a descending limit.
type EventQuery struct {
	ExamID     int64
	StudentNo  string   // empty: every student in the exam
	AfterID    int64    // exclusive lower bound on id
	Severities []string // empty: every severity
	Limit      int
	Ascending  bool
}

func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]EventRow, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	sb := strings.Builder{}
	sb.WriteString(`SELECT id, student_no, type, severity, payload_json,
		client_time, server_time FROM events WHERE exam_id=? AND id>?`)
	args := []any{q.ExamID, q.AfterID}
	if q.StudentNo != "" {
		sb.WriteString(" AND student_no=?")
		args = append(args, q.StudentNo)
	}
	if len(q.Severities) > 0 {
		sb.WriteString(" AND severity IN (" +
			strings.TrimSuffix(strings.Repeat("?,", len(q.Severities)), ",") + ")")
		for _, sev := range q.Severities {
			args = append(args, sev)
		}
	}
	if q.Ascending {
		sb.WriteString(" ORDER BY id ASC LIMIT ?")
	} else {
		sb.WriteString(" ORDER BY id DESC LIMIT ?")
	}
	args = append(args, q.Limit)

	rows, err := s.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []EventRow{}
	for rows.Next() {
		var x EventRow
		var payload string
		if err := rows.Scan(&x.ID, &x.StudentNo, &x.Type, &x.Severity, &payload,
			&x.ClientTime, &x.ServerTime); err != nil {
			return nil, err
		}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &x.Payload)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// EventTally is the per-student event count the dashboard shows beside a name,
// so a teacher scanning fifty rows sees who to look at without opening anyone.
type EventTally struct {
	Warn  int `json:"warn"`
	Alert int `json:"alert"`
}

func (s *Store) EventCountsByStudent(ctx context.Context, examID int64) (map[string]EventTally, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT student_no, severity, COUNT(*) FROM events
		WHERE exam_id=? AND severity IN ('warn','alert')
		GROUP BY student_no, severity`, examID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]EventTally{}
	for rows.Next() {
		var no, sev string
		var n int
		if err := rows.Scan(&no, &sev, &n); err != nil {
			return nil, err
		}
		t := out[no]
		if sev == protocol.SevAlert {
			t.Alert = n
		} else {
			t.Warn = n
		}
		out[no] = t
	}
	return out, rows.Err()
}

// CountFinals returns how many final submissions a student has already made
// for a problem. Checked before accepting a new one so a "no resubmission"
// exam rejects the attempt instead of storing it and then complaining.
func (s *Store) CountFinals(ctx context.Context, sessionID, problemID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM submissions
		WHERE session_id=? AND problem_id=? AND kind='final'`,
		sessionID, problemID).Scan(&n)
	return n, err
}

// SubmissionRow is the teacher's submission-list view: the submission joined
// with its judge result and queue state in one query.
type SubmissionRow struct {
	ID        int64  `json:"id"`
	StudentNo string `json:"student_no"`
	Name      string `json:"name"`
	ProblemID int64  `json:"problem_id"`
	Language  string `json:"language"`
	Kind      string `json:"kind"`
	Seq       int    `json:"seq"`
	CodeBytes int    `json:"code_bytes"`
	CreatedAt int64  `json:"created_at"`
	Verdict   string `json:"verdict"`
	Score     int    `json:"score"`
	MaxScore  int    `json:"max_score"`
	TimeMS    int    `json:"time_ms"`
	MemoryKB  int    `json:"memory_kb"`
	TaskState string `json:"task_state"`
	Promoted  bool   `json:"promoted"`
}

func (s *Store) ListSubmissionRows(ctx context.Context, examID int64, kind string, limit int) ([]SubmissionRow, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	query := `
		SELECT s.id, s.student_no, sess.name, s.problem_id, s.language, s.kind, s.seq,
		       s.code_bytes, s.created_at,
		       COALESCE(r.verdict,''), COALESCE(r.score,0), COALESCE(r.max_score,0),
		       COALESCE(r.time_ms,0), COALESCE(r.memory_kb,0), COALESCE(t.state,''),
		       s.promoted_from IS NOT NULL
		FROM submissions s
		JOIN sessions sess ON sess.id = s.session_id
		LEFT JOIN judge_results r ON r.submission_id = s.id
		LEFT JOIN judge_tasks  t ON t.submission_id = s.id
		WHERE s.exam_id=?`
	args := []any{examID}
	if kind != "" {
		query += ` AND s.kind=?`
		args = append(args, kind)
	}
	query += ` ORDER BY s.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SubmissionRow{}
	for rows.Next() {
		var x SubmissionRow
		if err := rows.Scan(&x.ID, &x.StudentNo, &x.Name, &x.ProblemID, &x.Language,
			&x.Kind, &x.Seq, &x.CodeBytes, &x.CreatedAt, &x.Verdict, &x.Score,
			&x.MaxScore, &x.TimeMS, &x.MemoryKB, &x.TaskState, &x.Promoted); err != nil {
			return nil, err
		}
		if x.Verdict == "" {
			x.Verdict = protocol.VerdictPending
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetJudgeResult loads one stored result, or nil when not judged yet.
func (s *Store) GetJudgeResult(ctx context.Context, submissionID int64) (*JudgeResult, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT submission_id, verdict, score, max_score, time_ms, memory_kb,
		       compile_log, detail_json, judged_at
		FROM judge_results WHERE submission_id=?`, submissionID)

	var r JudgeResult
	var detail string
	err := row.Scan(&r.SubmissionID, &r.Verdict, &r.Score, &r.MaxScore, &r.TimeMS,
		&r.MemoryKB, &r.CompileLog, &detail, &r.JudgedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if detail != "" {
		_ = json.Unmarshal([]byte(detail), &r.Cases)
	}
	if r.Cases == nil {
		r.Cases = []protocol.CaseResult{}
	}
	return &r, nil
}

// SnapshotTimeline lists every stored version of one student's answer to one
// problem, oldest first. This is the editing trail the teacher inspects when a
// paste event looks suspicious.
func (s *Store) SnapshotTimeline(ctx context.Context, sessionID, problemID int64) ([]SubmissionRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id, s.student_no, sess.name, s.problem_id, s.language, s.kind, s.seq,
		       s.code_bytes, s.created_at, '', 0, 0, 0, 0, '',
		       s.promoted_from IS NOT NULL
		FROM submissions s
		JOIN sessions sess ON sess.id = s.session_id
		WHERE s.session_id=? AND s.problem_id=?
		ORDER BY s.id`, sessionID, problemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SubmissionRow{}
	for rows.Next() {
		var x SubmissionRow
		if err := rows.Scan(&x.ID, &x.StudentNo, &x.Name, &x.ProblemID, &x.Language,
			&x.Kind, &x.Seq, &x.CodeBytes, &x.CreatedAt, &x.Verdict, &x.Score,
			&x.MaxScore, &x.TimeMS, &x.MemoryKB, &x.TaskState, &x.Promoted); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LatestFinal returns the newest final submission for one student+problem, or
// nil when they never submitted.
func (s *Store) LatestFinal(ctx context.Context, sessionID, problemID int64) (*Submission, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+submissionCols+` FROM submissions
		 WHERE session_id=? AND problem_id=? AND kind='final'
		 ORDER BY id DESC LIMIT 1`, sessionID, problemID)
	sub, err := scanSubmission(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return sub, err
}
