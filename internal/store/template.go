package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"
)

// How a template item chooses its problem.
const (
	PickFixed  = "fixed"  // always this problem
	PickPool   = "pool"   // one at random from a teacher-listed set
	PickFilter = "filter" // one at random from everything matching a filter
)

// ProblemFilter selects candidates by difficulty and knowledge-point tags.
//
// Pinning the difficulty per item is what keeps per-student papers comparable:
// everyone's question 3 is drawn from the same difficulty band, so the marks
// mean the same thing across students.
type ProblemFilter struct {
	MinDifficulty  int      `json:"min_difficulty"` // 0 = no lower bound
	MaxDifficulty  int      `json:"max_difficulty"` // 0 = no upper bound
	Tags           []string `json:"tags"`
	RequireAllTags bool     `json:"require_all_tags"` // false = any tag matches
}

type TemplateItem struct {
	ID             int64         `json:"id"`
	Idx            int           `json:"idx"`
	PickType       string        `json:"pick_type"`
	FixedProblemID *int64        `json:"fixed_problem_id"`
	Pool           []int64       `json:"pool"`
	Filter         ProblemFilter `json:"filter"`
	Score          int           `json:"score"`
}

type PaperTemplate struct {
	ID         int64          `json:"id"`
	Name       string         `json:"name"`
	TotalScore int            `json:"total_score"`
	SeedNonce  int64          `json:"seed_nonce"`
	Items      []TemplateItem `json:"items"`
	CreatedAt  int64          `json:"created_at"`
}

// templateRules is what goes in paper_templates.rules_json.
type templateRules struct {
	// SeedNonce lets a teacher re-roll the draw. Generation is otherwise
	// deterministic in (exam, student), so without this a regenerated paper
	// would always come out identical.
	SeedNonce int64 `json:"seed_nonce"`
}

// Candidate is a problem as far as paper generation is concerned.
type Candidate struct {
	ID          int64
	Difficulty  int
	Tags        []string
	HasTestData bool
}

func (c Candidate) hasTag(tag string) bool {
	for _, t := range c.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

// eligible reports whether this candidate can fill the given item.
func eligible(item TemplateItem, c Candidate, used map[int64]bool) bool {
	if used[c.ID] {
		return false // never the same problem twice on one paper
	}
	// A problem with no test data judges as a system error. Letting a random
	// draw land on one would hand a student an unanswerable question, so they
	// are excluded from every random pick. A fixed item is the teacher's
	// explicit choice and is honoured, with a warning.
	if !c.HasTestData {
		return false
	}
	switch item.PickType {
	case PickPool:
		for _, id := range item.Pool {
			if id == c.ID {
				return true
			}
		}
		return false
	case PickFilter:
		f := item.Filter
		if f.MinDifficulty > 0 && c.Difficulty < f.MinDifficulty {
			return false
		}
		if f.MaxDifficulty > 0 && c.Difficulty > f.MaxDifficulty {
			return false
		}
		if len(f.Tags) == 0 {
			return true
		}
		if f.RequireAllTags {
			for _, t := range f.Tags {
				if !c.hasTag(t) {
					return false
				}
			}
			return true
		}
		for _, t := range f.Tags {
			if c.hasTag(t) {
				return true
			}
		}
		return false
	}
	return false
}

// GenerationResult is one generated paper plus anything the teacher should know.
type GenerationResult struct {
	Items    []PaperItemInput `json:"items"`
	Warnings []string         `json:"warnings"`
}

// generatePaper picks one problem per template item.
//
// Deterministic in seed: the same (template, exam, student) always yields the
// same paper, so a student who restarts their client does not get a different
// question set, and a teacher can preview exactly what a student will see.
func generatePaper(items []TemplateItem, pool []Candidate, seed int64) (*GenerationResult, error) {
	// Candidate order must be stable or the draw would not be reproducible.
	sorted := append([]Candidate(nil), pool...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	rng := rand.New(rand.NewSource(seed))
	used := map[int64]bool{}
	out := &GenerationResult{Items: []PaperItemInput{}, Warnings: []string{}}

	byID := make(map[int64]Candidate, len(sorted))
	for _, c := range sorted {
		byID[c.ID] = c
	}

	for _, item := range items {
		score := item.Score
		if score <= 0 {
			score = 10
		}

		if item.PickType == PickFixed {
			if item.FixedProblemID == nil {
				return nil, fmt.Errorf("第 %d 题指定为固定题目，但没有选择题目", item.Idx)
			}
			id := *item.FixedProblemID
			if c, ok := byID[id]; !ok {
				return nil, fmt.Errorf("第 %d 题指定的题目 #%d 不存在", item.Idx, id)
			} else if !c.HasTestData {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("第 %d 题（固定题目 #%d）没有测试数据，提交会判为系统错误", item.Idx, id))
			}
			if used[id] {
				out.Warnings = append(out.Warnings,
					fmt.Sprintf("第 %d 题与前面的题目重复（#%d）", item.Idx, id))
			}
			used[id] = true
			out.Items = append(out.Items, PaperItemInput{ProblemID: id, Score: score})
			continue
		}

		var choices []Candidate
		for _, c := range sorted {
			if eligible(item, c, used) {
				choices = append(choices, c)
			}
		}
		if len(choices) == 0 {
			return nil, fmt.Errorf("第 %d 题没有可用题目：%s", item.Idx, explainEmpty(item))
		}
		pick := choices[rng.Intn(len(choices))]
		used[pick.ID] = true
		out.Items = append(out.Items, PaperItemInput{ProblemID: pick.ID, Score: score})
	}
	return out, nil
}

// DescribeRule states an item's selection rule neutrally, for the template
// preview. It must not say anything about whether candidates exist - the
// preview shows the count separately, and an earlier version that folded the
// failure wording in here printed "no match" next to a list of matches.
func DescribeRule(item TemplateItem) string {
	switch item.PickType {
	case PickFixed:
		if item.FixedProblemID == nil {
			return "未选择题目"
		}
		return fmt.Sprintf("固定题目 #%d", *item.FixedProblemID)
	case PickPool:
		return fmt.Sprintf("从指定的 %d 道题中随机抽 1 道", len(item.Pool))
	case PickFilter:
		f := item.Filter
		parts := []string{}
		if f.MinDifficulty > 0 || f.MaxDifficulty > 0 {
			lo, hi := f.MinDifficulty, f.MaxDifficulty
			if lo == 0 {
				lo = 1
			}
			if hi == 0 {
				hi = 5
			}
			if lo == hi {
				parts = append(parts, fmt.Sprintf("难度 %d", lo))
			} else {
				parts = append(parts, fmt.Sprintf("难度 %d-%d", lo, hi))
			}
		}
		if len(f.Tags) > 0 {
			join := "任一"
			if f.RequireAllTags {
				join = "全部"
			}
			parts = append(parts, fmt.Sprintf("标签%s匹配 [%s]", join, strings.Join(f.Tags, "、")))
		}
		if len(parts) == 0 {
			return "全部题目中随机抽 1 道"
		}
		return "随机抽 1 道，条件：" + strings.Join(parts, " + ")
	}
	return "未知的取题方式 " + item.PickType
}

// explainEmpty says why an item could not be filled, which is a different
// question from what its rule is.
func explainEmpty(item TemplateItem) string {
	switch item.PickType {
	case PickPool:
		return fmt.Sprintf("题目池中的 %d 道题都不可用（可能已被前面的题位选走，或没有测试数据）",
			len(item.Pool))
	case PickFilter:
		return DescribeRule(item) + "：没有题目同时满足条件、尚未被选走、且有测试数据"
	}
	return DescribeRule(item)
}

// paperSeed derives the draw for one student. Hashing the student number means
// the same student always gets the same paper, and different students get
// independent draws, without storing a seed per student.
func paperSeed(templateNonce, examID int64, studentNo string) int64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%d|%s", templateNonce, examID, studentNo)
	return int64(h.Sum64())
}

// ---------------------------------------------------------------- templates

func (t *PaperTemplate) normalize() {
	for i := range t.Items {
		t.Items[i].Idx = i + 1
		if t.Items[i].Score <= 0 {
			t.Items[i].Score = 10
		}
		switch t.Items[i].PickType {
		case PickFixed, PickPool, PickFilter:
		default:
			t.Items[i].PickType = PickFixed
		}
		if t.Items[i].Pool == nil {
			t.Items[i].Pool = []int64{}
		}
	}
	t.TotalScore = 0
	for _, it := range t.Items {
		t.TotalScore += it.Score
	}
	if t.SeedNonce == 0 {
		t.SeedNonce = NowMS()
	}
}

func (s *Store) SaveTemplate(ctx context.Context, t *PaperTemplate) error {
	t.normalize()
	rules, err := json.Marshal(templateRules{SeedNonce: t.SeedNonce})
	if err != nil {
		return err
	}
	mode := "fixed"
	for _, it := range t.Items {
		if it.PickType != PickFixed {
			mode = "random"
			break
		}
	}
	now := NowMS()

	return s.write(ctx, func(tx *sql.Tx) error {
		if t.ID == 0 {
			res, err := tx.ExecContext(ctx, `
				INSERT INTO paper_templates(name, mode, total_score, rules_json, created_at)
				VALUES(?,?,?,?,?)`, t.Name, mode, t.TotalScore, string(rules), now)
			if err != nil {
				return err
			}
			if t.ID, err = res.LastInsertId(); err != nil {
				return err
			}
			t.CreatedAt = now
		} else {
			if _, err := tx.ExecContext(ctx, `
				UPDATE paper_templates SET name=?, mode=?, total_score=?, rules_json=?
				WHERE id=?`, t.Name, mode, t.TotalScore, string(rules), t.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM template_items WHERE template_id=?`, t.ID); err != nil {
				return err
			}
		}
		for _, it := range t.Items {
			poolJSON, err := json.Marshal(it.Pool)
			if err != nil {
				return err
			}
			filterJSON, err := json.Marshal(it.Filter)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO template_items(template_id, idx, pick_type, fixed_problem_id,
				  pool_json, filter_json, score)
				VALUES(?,?,?,?,?,?,?)`,
				t.ID, it.Idx, it.PickType, it.FixedProblemID,
				string(poolJSON), string(filterJSON), it.Score); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) GetTemplate(ctx context.Context, id int64) (*PaperTemplate, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, total_score, rules_json, created_at FROM paper_templates WHERE id=?`, id)

	var t PaperTemplate
	var rules string
	if err := row.Scan(&t.ID, &t.Name, &t.TotalScore, &rules, &t.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if rules != "" {
		var r templateRules
		if json.Unmarshal([]byte(rules), &r) == nil {
			t.SeedNonce = r.SeedNonce
		}
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, idx, pick_type, fixed_problem_id, pool_json, filter_json, score
		FROM template_items WHERE template_id=? ORDER BY idx`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	t.Items = []TemplateItem{}
	for rows.Next() {
		var it TemplateItem
		var poolJSON, filterJSON string
		if err := rows.Scan(&it.ID, &it.Idx, &it.PickType, &it.FixedProblemID,
			&poolJSON, &filterJSON, &it.Score); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(poolJSON), &it.Pool)
		_ = json.Unmarshal([]byte(filterJSON), &it.Filter)
		if it.Pool == nil {
			it.Pool = []int64{}
		}
		t.Items = append(t.Items, it)
	}
	return &t, rows.Err()
}

func (s *Store) ListTemplates(ctx context.Context) ([]*PaperTemplate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM paper_templates ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := []*PaperTemplate{}
	for _, id := range ids {
		t, err := s.GetTemplate(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (s *Store) DeleteTemplate(ctx context.Context, id int64) error {
	_, err := s.exec(ctx, `DELETE FROM paper_templates WHERE id=?`, id)
	return err
}

// RerollTemplate changes the seed so the next generation draws differently.
func (s *Store) RerollTemplate(ctx context.Context, id int64) error {
	rules, err := json.Marshal(templateRules{SeedNonce: NowMS()})
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, `UPDATE paper_templates SET rules_json=? WHERE id=?`, string(rules), id)
	return err
}

// Candidates loads every problem as a generation candidate.
func (s *Store) Candidates(ctx context.Context) ([]Candidate, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.difficulty, p.tags,
		       (SELECT COUNT(*) FROM testcases t WHERE t.problem_id=p.id)
		FROM problems p ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		var tags string
		var caseCount int
		if err := rows.Scan(&c.ID, &c.Difficulty, &tags, &caseCount); err != nil {
			return nil, err
		}
		c.Tags = splitCSV(tags)
		c.HasTestData = caseCount > 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// GeneratePaperFor produces the paper a given student would sit. Pass an empty
// studentNo for a class-wide paper.
func (s *Store) GeneratePaperFor(ctx context.Context, templateID, examID int64, studentNo string) (*GenerationResult, error) {
	t, err := s.GetTemplate(ctx, templateID)
	if err != nil {
		return nil, err
	}
	if len(t.Items) == 0 {
		return nil, fmt.Errorf("试卷模板「%s」还没有题目", t.Name)
	}
	pool, err := s.Candidates(ctx)
	if err != nil {
		return nil, err
	}
	return generatePaper(t.Items, pool, paperSeed(t.SeedNonce, examID, studentNo))
}

// EligibleCandidates lists everything this item could draw, ignoring problems
// already used elsewhere on the paper. The teacher's preview uses it to show
// how much room a rule actually has.
func EligibleCandidates(item TemplateItem, pool []Candidate) []Candidate {
	out := []Candidate{}
	none := map[int64]bool{}
	for _, c := range pool {
		if item.PickType == PickFixed {
			if item.FixedProblemID != nil && c.ID == *item.FixedProblemID {
				out = append(out, c)
			}
			continue
		}
		if eligible(item, c, none) {
			out = append(out, c)
		}
	}
	return out
}
