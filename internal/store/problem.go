package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"coderoom/internal/protocol"
)

var ErrNotFound = errors.New("not found")

// Score modes for a problem.
const (
	ScoreModeSum          = "sum"          // sum of passed test cases
	ScoreModeAllOrNothing = "allornothing" // full marks only if every case passes
)

type Problem struct {
	ID            int64             `json:"id"`
	Title         string            `json:"title"`
	DescriptionMD string            `json:"description_md"`
	InputDesc     string            `json:"input_desc"`
	OutputDesc    string            `json:"output_desc"`
	Samples       []protocol.Sample `json:"samples"`
	TimeLimitMS   int               `json:"time_limit_ms"`
	MemoryLimitMB int               `json:"memory_limit_mb"`
	Difficulty    int               `json:"difficulty"`
	Tags          []string          `json:"tags"`
	Languages     []string          `json:"languages"`
	ScoreMode     string            `json:"score_mode"`
	// FloatEps is the tolerance for numeric output; 0 means an exact match.
	// Set it on problems whose answer is a real number, where demanding the
	// same digits would fail every correct submission.
	FloatEps  float64 `json:"float_eps"`
	CreatedAt int64   `json:"created_at"`
	UpdatedAt int64   `json:"updated_at"`

	// Derived, filled by List/Get for the teacher UI.
	CaseCount int `json:"case_count"`
	CaseScore int `json:"case_score"`
}

// ToProtocol strips everything the student must not see (test data lives in
// separate rows, but this also drops tags/difficulty/score mode).
func (p *Problem) ToProtocol() *protocol.Problem {
	return &protocol.Problem{
		ID:            p.ID,
		Title:         p.Title,
		DescriptionMD: p.DescriptionMD,
		InputDesc:     p.InputDesc,
		OutputDesc:    p.OutputDesc,
		Samples:       p.Samples,
		TimeLimitMS:   p.TimeLimitMS,
		MemoryLimitMB: p.MemoryLimitMB,
		Languages:     p.Languages,
	}
}

type Testcase struct {
	ID         int64  `json:"id"`
	ProblemID  int64  `json:"problem_id"`
	Idx        int    `json:"idx"`
	InputPath  string `json:"-"`
	OutputPath string `json:"-"`
	Score      int    `json:"score"`
	IsSample   bool   `json:"is_sample"`
}

// TestcaseInput is a case as supplied by the teacher UI, contents inline.
type TestcaseInput struct {
	Input    string `json:"input"`
	Output   string `json:"output"`
	Score    int    `json:"score"`
	IsSample bool   `json:"is_sample"`
}

func splitCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (p *Problem) normalize() {
	if p.TimeLimitMS <= 0 {
		p.TimeLimitMS = 1000
	}
	if p.MemoryLimitMB <= 0 {
		p.MemoryLimitMB = 256
	}
	if p.Difficulty < 1 || p.Difficulty > 5 {
		p.Difficulty = 2
	}
	if len(p.Languages) == 0 {
		p.Languages = []string{protocol.LangCPP, protocol.LangC, protocol.LangPython}
	}
	// A negative or absurd tolerance would silently accept anything.
	if p.FloatEps < 0 || p.FloatEps > 1 {
		p.FloatEps = 0
	}
	if p.ScoreMode != ScoreModeAllOrNothing {
		p.ScoreMode = ScoreModeSum
	}
	if p.Samples == nil {
		p.Samples = []protocol.Sample{}
	}
}

func (s *Store) CreateProblem(ctx context.Context, p *Problem) (int64, error) {
	p.normalize()
	samples, err := json.Marshal(p.Samples)
	if err != nil {
		return 0, err
	}
	now := NowMS()
	res, err := s.exec(ctx, `
		INSERT INTO problems(title, description_md, input_desc, output_desc, samples_json,
		  time_limit_ms, memory_limit_mb, difficulty, tags, languages, score_mode,
		  float_eps, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.Title, p.DescriptionMD, p.InputDesc, p.OutputDesc, string(samples),
		p.TimeLimitMS, p.MemoryLimitMB, p.Difficulty,
		strings.Join(p.Tags, ","), strings.Join(p.Languages, ","), p.ScoreMode,
		p.FloatEps, now, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	p.ID, p.CreatedAt, p.UpdatedAt = id, now, now
	return id, nil
}

func (s *Store) UpdateProblem(ctx context.Context, p *Problem) error {
	p.normalize()
	samples, err := json.Marshal(p.Samples)
	if err != nil {
		return err
	}
	res, err := s.exec(ctx, `
		UPDATE problems SET title=?, description_md=?, input_desc=?, output_desc=?,
		  samples_json=?, time_limit_ms=?, memory_limit_mb=?, difficulty=?, tags=?,
		  languages=?, score_mode=?, float_eps=?, updated_at=?
		WHERE id=?`,
		p.Title, p.DescriptionMD, p.InputDesc, p.OutputDesc, string(samples),
		p.TimeLimitMS, p.MemoryLimitMB, p.Difficulty,
		strings.Join(p.Tags, ","), strings.Join(p.Languages, ","), p.ScoreMode,
		p.FloatEps, NowMS(), p.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const problemCols = `p.id, p.title, p.description_md, p.input_desc, p.output_desc,
	p.samples_json, p.time_limit_ms, p.memory_limit_mb, p.difficulty, p.tags,
	p.languages, p.score_mode, p.float_eps, p.created_at, p.updated_at,
	(SELECT COUNT(*) FROM testcases t WHERE t.problem_id=p.id),
	(SELECT COALESCE(SUM(t.score),0) FROM testcases t WHERE t.problem_id=p.id)`

func scanProblem(sc interface{ Scan(...any) error }) (*Problem, error) {
	var p Problem
	var samples, tags, langs string
	err := sc.Scan(&p.ID, &p.Title, &p.DescriptionMD, &p.InputDesc, &p.OutputDesc,
		&samples, &p.TimeLimitMS, &p.MemoryLimitMB, &p.Difficulty, &tags,
		&langs, &p.ScoreMode, &p.FloatEps, &p.CreatedAt, &p.UpdatedAt,
		&p.CaseCount, &p.CaseScore)
	if err != nil {
		return nil, err
	}
	if samples != "" {
		_ = json.Unmarshal([]byte(samples), &p.Samples)
	}
	if p.Samples == nil {
		p.Samples = []protocol.Sample{}
	}
	p.Tags = splitCSV(tags)
	p.Languages = splitCSV(langs)
	return &p, nil
}

func (s *Store) GetProblem(ctx context.Context, id int64) (*Problem, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+problemCols+` FROM problems p WHERE p.id=?`, id)
	p, err := scanProblem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) ListProblems(ctx context.Context) ([]*Problem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+problemCols+` FROM problems p ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Problem{}
	for rows.Next() {
		p, err := scanProblem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteProblem(ctx context.Context, id int64) error {
	if _, err := s.exec(ctx, `DELETE FROM problems WHERE id=?`, id); err != nil {
		return err
	}
	return os.RemoveAll(s.ProblemDir(id))
}

// ReplaceTestcases rewrites a problem's entire test data set: files on disk
// first, then rows in one transaction. Cases are fully replaced rather than
// merged so the teacher UI never has to reason about partial state.
func (s *Store) ReplaceTestcases(ctx context.Context, problemID int64, cases []TestcaseInput) error {
	dir := s.ProblemDir(problemID)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	type written struct {
		in, out string
		score   int
		sample  bool
	}
	files := make([]written, 0, len(cases))
	for i, c := range cases {
		inPath := filepath.Join(dir, fmt.Sprintf("%d.in", i+1))
		outPath := filepath.Join(dir, fmt.Sprintf("%d.out", i+1))
		if err := os.WriteFile(inPath, []byte(c.Input), 0o644); err != nil {
			return err
		}
		if err := os.WriteFile(outPath, []byte(c.Output), 0o644); err != nil {
			return err
		}
		score := c.Score
		if score <= 0 {
			score = 10
		}
		files = append(files, written{inPath, outPath, score, c.IsSample})
	}

	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM testcases WHERE problem_id=?`, problemID); err != nil {
			return err
		}
		for i, f := range files {
			sample := 0
			if f.sample {
				sample = 1
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO testcases(problem_id, idx, input_path, output_path, score, is_sample)
				VALUES(?,?,?,?,?,?)`,
				problemID, i+1, f.in, f.out, f.score, sample)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) ListTestcases(ctx context.Context, problemID int64) ([]Testcase, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, problem_id, idx, input_path, output_path, score, is_sample
		FROM testcases WHERE problem_id=? ORDER BY idx`, problemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Testcase{}
	for rows.Next() {
		var t Testcase
		var sample int
		if err := rows.Scan(&t.ID, &t.ProblemID, &t.Idx, &t.InputPath,
			&t.OutputPath, &t.Score, &sample); err != nil {
			return nil, err
		}
		t.IsSample = sample != 0
		out = append(out, t)
	}
	return out, rows.Err()
}
