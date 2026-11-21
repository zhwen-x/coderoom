package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
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
	client_version, token, paper_id, login_at, last_heartbeat_at, submitted_at`

func scanSession(sc interface{ Scan(...any) error }) (*Session, error) {
	var x Session
	err := sc.Scan(&x.ID, &x.ExamID, &x.StudentNo, &x.Name, &x.Class, &x.MachineName,
		&x.IP, &x.MAC, &x.OS, &x.ClientVersion, &x.Token, &x.PaperID,
		&x.LoginAt, &x.LastHeartbeatAt, &x.SubmittedAt)
	if err != nil {
		return nil, err
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
func (s *Store) Heartbeat(ctx context.Context, sessionID int64) error {
	_, err := s.exec(ctx,
		`UPDATE sessions SET last_heartbeat_at=? WHERE id=?`, NowMS(), sessionID)
	return err
}

// MarkSubmitted stamps the moment a student finished (hand-in confirmed).
func (s *Store) MarkSubmitted(ctx context.Context, sessionID int64) error {
	_, err := s.exec(ctx,
		`UPDATE sessions SET submitted_at=COALESCE(submitted_at, ?) WHERE id=?`,
		NowMS(), sessionID)
	return err
}
