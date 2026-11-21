// Package store owns all persistence: the SQLite database plus the on-disk
// layout for test data and submitted code.
//
// Concurrency model: SQLite in WAL mode handles concurrent readers natively,
// but only one writer at a time. Rather than serialising everything onto a
// single connection (which would make a slow read block all writes), reads run
// concurrently and writes are serialised with a mutex. At the 50-client scale
// this project targets, writes are a trickle (~2/s from snapshots), so this is
// ample; the mutex is also the natural seam to replace if the judge is ever
// split onto its own machine.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// Store is the handle to all persistent state.
type Store struct {
	db      *sql.DB
	dataDir string
	wmu     sync.Mutex // serialises write transactions
}

// NowMS returns the current time as unix milliseconds, the unit used for every
// timestamp in the schema and on the wire.
func NowMS() int64 { return time.Now().UnixMilli() }

// Open prepares dataDir and opens (creating if needed) the database inside it.
func Open(dataDir string) (*Store, error) {
	for _, sub := range []string{"", "problems", "submissions", "exports"} {
		if err := os.MkdirAll(filepath.Join(dataDir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}

	dbPath := filepath.Join(dataDir, "coderoom.db")
	dsn := "file:" + dbPath + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	s := &Store{db: db, dataDir: dataDir}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DataDir is the root of the on-disk layout (test data, code, exports).
func (s *Store) DataDir() string { return s.dataDir }

func (s *Store) migrate() error {
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if _, err := s.db.Exec(string(schema)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	_, err = s.db.Exec(
		`INSERT INTO meta(key, value) VALUES('schema_version','1')
		 ON CONFLICT(key) DO NOTHING`)
	if err != nil {
		return fmt.Errorf("stamp schema version: %w", err)
	}
	return nil
}

// write runs fn inside a serialised write transaction.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// exec runs a single serialised write statement.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	return s.db.ExecContext(ctx, query, args...)
}

// ProblemDir is where a problem's test data lives.
func (s *Store) ProblemDir(problemID int64) string {
	return filepath.Join(s.dataDir, "problems", fmt.Sprint(problemID))
}

// SubmissionDir is where one student's code for one problem is archived. Every
// snapshot and final submission is kept, which is what makes the editing
// timeline (and the anti-plagiarism evidence trail) possible.
func (s *Store) SubmissionDir(examID int64, studentNo string, problemID int64) string {
	return filepath.Join(s.dataDir, "submissions",
		fmt.Sprint(examID), sanitize(studentNo), fmt.Sprint(problemID))
}

// sanitize makes a student-supplied identifier safe to use as a path element.
// Student numbers come from an untrusted client, so this is a security
// boundary, not a cosmetic one.
func sanitize(s string) string {
	if s == "" {
		return "_"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

// LogAudit records a teacher-side action for after-the-fact review.
func (s *Store) LogAudit(ctx context.Context, actor, action, target string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `
		INSERT INTO audit_logs(actor, action, target, payload_json, created_at)
		VALUES(?,?,?,?,?)`, actor, action, target, string(b), NowMS())
	return err
}
