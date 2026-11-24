package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"coderoom/internal/protocol"
)

type Student struct {
	ID        int64  `json:"id"`
	StudentNo string `json:"student_no"`
	Name      string `json:"name"`
	Class     string `json:"class"`
	Batch     string `json:"batch"`
	CreatedAt int64  `json:"created_at"`
}

// ImportStudents upserts a roster batch. Re-importing the same student number
// updates the name and class rather than failing, so a teacher can fix a typo
// by re-importing the corrected file.
func (s *Store) ImportStudents(ctx context.Context, list []Student, batch string) (int, error) {
	now := NowMS()
	n := 0
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, st := range list {
			st.StudentNo = strings.TrimSpace(st.StudentNo)
			st.Name = strings.TrimSpace(st.Name)
			if st.StudentNo == "" || st.Name == "" {
				continue
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO students(student_no, name, class, batch, created_at)
				VALUES(?,?,?,?,?)
				ON CONFLICT(student_no) DO UPDATE SET
				  name=excluded.name, class=excluded.class, batch=excluded.batch`,
				st.StudentNo, st.Name, strings.TrimSpace(st.Class), batch, now)
			if err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func (s *Store) FindStudent(ctx context.Context, studentNo string) (*Student, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, student_no, name, class, batch, created_at FROM students WHERE student_no=?`,
		studentNo)
	var st Student
	err := row.Scan(&st.ID, &st.StudentNo, &st.Name, &st.Class, &st.Batch, &st.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &st, err
}

func (s *Store) ListStudents(ctx context.Context) ([]Student, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, student_no, name, class, batch, created_at FROM students ORDER BY class, student_no`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Student{}
	for rows.Next() {
		var st Student
		if err := rows.Scan(&st.ID, &st.StudentNo, &st.Name, &st.Class,
			&st.Batch, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

type Session struct {
	ID              int64  `json:"id"`
	ExamID          int64  `json:"exam_id"`
	StudentNo       string `json:"student_no"`
	Name            string `json:"name"`
	Class           string `json:"class"`
	MachineName     string `json:"machine_name"`
	IP              string `json:"ip"`
	MAC             string `json:"mac"`
	OS              string `json:"os"`
	ClientVersion   string `json:"client_version"`
	Token           string `json:"-"` // never serialised to the teacher UI
	PaperID         *int64 `json:"paper_id"`
	LoginAt         int64  `json:"login_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	SubmittedAt     *int64 `json:"submitted_at"`
	// Focused and Foreground are the client's last report of what the student
	// is looking at. Events say when a switch happened; these say what is true
	// now, which is the question an invigilator walking the room is asking.
	Focused    bool   `json:"focused"`
	Foreground string `json:"foreground"`
	// Guard is what this machine reports it is actually enforcing. Nil means
	// the client has not said yet - which for an exam that asked for
	// enforcement is itself worth showing.
	Guard *protocol.GuardReport `json:"guard"`
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LoginResult carries the session plus anything the caller should record as an
// invigilation event.
type LoginResult struct {
	Session *Session
	// PrevMachine is non-empty when this student number was last seen on a
	// different machine. That is the signal for an impersonation / multi-seat
	// alert; login still succeeds so a genuine machine swap is not blocked
	// mid-exam, but the teacher is told.
	PrevMachine string
}

// Login creates or resumes the single session a student holds for an exam.
// Resuming (rather than creating a second row) is what lets a student restart
// a crashed client and pick up their drafts.
func (s *Store) Login(ctx context.Context, sess *Session) (*LoginResult, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	sess.Token = token
	now := NowMS()
	res := &LoginResult{Session: sess}

	err = s.write(ctx, func(tx *sql.Tx) error {
		var prevID int64
		var prevMachine string
		row := tx.QueryRowContext(ctx,
			`SELECT id, machine_name FROM sessions WHERE exam_id=? AND student_no=?`,
			sess.ExamID, sess.StudentNo)
		switch err := row.Scan(&prevID, &prevMachine); {
		case err == nil:
			if prevMachine != "" && sess.MachineName != "" && prevMachine != sess.MachineName {
				res.PrevMachine = prevMachine
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE sessions SET name=?, class=?, machine_name=?, ip=?, mac=?, os=?,
				  client_version=?, token=?, paper_id=?, last_heartbeat_at=?
				WHERE id=?`,
				sess.Name, sess.Class, sess.MachineName, sess.IP, sess.MAC, sess.OS,
				sess.ClientVersion, sess.Token, sess.PaperID, now, prevID)
			if err != nil {
				return err
			}
			sess.ID = prevID
			return tx.QueryRowContext(ctx,
				`SELECT login_at, submitted_at FROM sessions WHERE id=?`, prevID).
				Scan(&sess.LoginAt, &sess.SubmittedAt)
		case errors.Is(err, sql.ErrNoRows):
			ins, err := tx.ExecContext(ctx, `
				INSERT INTO sessions(exam_id, student_no, name, class, machine_name, ip,
				  mac, os, client_version, token, paper_id, login_at, last_heartbeat_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
				sess.ExamID, sess.StudentNo, sess.Name, sess.Class, sess.MachineName,
				sess.IP, sess.MAC, sess.OS, sess.ClientVersion, sess.Token,
				sess.PaperID, now, now)
			if err != nil {
				return err
			}
			if sess.ID, err = ins.LastInsertId(); err != nil {
				return err
			}
			sess.LoginAt = now
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return nil, err
	}
	sess.LastHeartbeatAt = now
	return res, nil
}

const sessionCols = `id, exam_id, student_no, name, class, machine_name, ip, mac, os,
	client_version, token, paper_id, login_at, last_heartbeat_at, submitted_at,
	focused, foreground, guard_json`

// sessionColsPrefixed is the same list qualified for a query that joins, where
// a bare column name would be ambiguous.
var sessionColsPrefixed = func() string {
	parts := strings.Split(sessionCols, ",")
	for i, p := range parts {
		parts[i] = "s." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}()

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	var focused int
	var guardJSON string
	err := sc.Scan(&x.ID, &x.ExamID, &x.StudentNo, &x.Name, &x.Class, &x.MachineName,
		&x.IP, &x.MAC, &x.OS, &x.ClientVersion, &x.Token, &x.PaperID,
		&x.LoginAt, &x.LastHeartbeatAt, &x.SubmittedAt, &focused, &x.Foreground, &guardJSON)
	if err != nil {
		return nil, err
	}
	x.Focused = focused != 0
	if guardJSON != "" && guardJSON != "{}" {
		var g protocol.GuardReport
		if json.Unmarshal([]byte(guardJSON), &g) == nil {
			x.Guard = &g
		}
	}
	return &x, nil
}

func (s *Store) SessionByToken(ctx context.Context, token string) (*Session, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE token=?`, token)
	x, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return x, err
}

func (s *Store) ListSessions(ctx context.Context, examID int64) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE exam_id=? ORDER BY student_no`, examID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Session{}
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// Heartbeat records liveness. The server detects a killed client by the
// absence of these, since a killed client cannot report its own death.
//
// Clearing offline_alerted here is what makes the lost-heartbeat alert
// episodic: a student whose machine comes back is eligible to be reported
// again if it goes away a second time.
func (s *Store) Heartbeat(ctx context.Context, sessionID int64) error {
	_, err := s.exec(ctx,
		`UPDATE sessions SET last_heartbeat_at=?, offline_alerted=0 WHERE id=?`,
		NowMS(), sessionID)
	return err
}

// RecordClientState is the heartbeat plus what the client reported about the
// student's screen and about the controls it is enforcing.
func (s *Store) RecordClientState(ctx context.Context, sessionID int64,
	focused bool, foreground string, guard *protocol.GuardReport) error {
	blob := "{}"
	if guard != nil {
		if raw, err := json.Marshal(guard); err == nil {
			blob = string(raw)
		}
	}
	_, err := s.exec(ctx, `
		UPDATE sessions SET last_heartbeat_at=?, offline_alerted=0,
		  focused=?, foreground=?, guard_json=? WHERE id=?`,
		NowMS(), b2i(focused), foreground, blob, sessionID)
	return err
}

// ClaimSilentSessions returns the sessions whose last heartbeat predates
// cutoff and marks them as alerted, in one transaction, so a caller can log
// exactly one event per episode even with several sweeps overlapping.
//
// The cutoff is passed in rather than derived from a grace period here: how
// long silence has to last before it means something is the server's policy,
// and keeping the clock out of this method is also what lets a test age a
// session without waiting for one.
//
// Scope is deliberately narrow. Only a running exam qualifies: during practice
// a student closing the client is just leaving, and after an exam ends every
// client in the room disconnects at once - alerting on either would produce a
// wall of events that means nothing. A student who has handed in is excluded
// for the same reason.
func (s *Store) ClaimSilentSessions(ctx context.Context, cutoff int64) ([]*Session, error) {
	var out []*Session
	err := s.write(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT `+sessionColsPrefixed+` FROM sessions s
			  JOIN exams e ON e.id = s.exam_id
			WHERE s.offline_alerted = 0
			  AND s.submitted_at IS NULL
			  AND s.last_heartbeat_at < ?
			  AND e.type = 'exam' AND e.state = 'running'`, cutoff)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			x, err := scanSession(rows)
			if err != nil {
				return err
			}
			out = append(out, x)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for _, x := range out {
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET offline_alerted=1 WHERE id=?`, x.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkSubmitted stamps the moment a student finished (hand-in confirmed).
func (s *Store) MarkSubmitted(ctx context.Context, sessionID int64) error {
	_, err := s.exec(ctx,
		`UPDATE sessions SET submitted_at=COALESCE(submitted_at, ?) WHERE id=?`,
		NowMS(), sessionID)
	return err
}
