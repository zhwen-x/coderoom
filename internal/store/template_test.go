package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// bank builds a candidate set with a known difficulty/tag spread.
func bank() []Candidate {
	return []Candidate{
		{ID: 1, Difficulty: 1, Tags: []string{"顺序结构"}, HasTestData: true},
		{ID: 2, Difficulty: 1, Tags: []string{"顺序结构"}, HasTestData: true},
		{ID: 3, Difficulty: 2, Tags: []string{"分支"}, HasTestData: true},
		{ID: 4, Difficulty: 2, Tags: []string{"分支", "循环"}, HasTestData: true},
		{ID: 5, Difficulty: 3, Tags: []string{"循环"}, HasTestData: true},
		{ID: 6, Difficulty: 3, Tags: []string{"循环", "数组"}, HasTestData: true},
		{ID: 7, Difficulty: 4, Tags: []string{"数组"}, HasTestData: true},
		{ID: 8, Difficulty: 5, Tags: []string{"递归"}, HasTestData: true},
		{ID: 9, Difficulty: 2, Tags: []string{"分支"}, HasTestData: false}, // 无测试数据
	}
}

func ids(items []PaperItemInput) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.ProblemID)
	}
	return out
}

func TestGenerateFixedPick(t *testing.T) {
	five := int64(5)
	res, err := generatePaper([]TemplateItem{
		{Idx: 1, PickType: PickFixed, FixedProblemID: &five, Score: 30},
	}, bank(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].ProblemID != 5 || res.Items[0].Score != 30 {
		t.Fatalf("got %+v, want problem 5 worth 30", res.Items)
	}
}

func TestGeneratePoolPickStaysInPool(t *testing.T) {
	pool := []int64{3, 4, 5}
	// Every seed must land inside the pool, not merely most of them.
	for seed := int64(0); seed < 50; seed++ {
		res, err := generatePaper([]TemplateItem{
			{Idx: 1, PickType: PickPool, Pool: pool, Score: 10},
		}, bank(), seed)
		if err != nil {
			t.Fatal(err)
		}
		got := res.Items[0].ProblemID
		if got != 3 && got != 4 && got != 5 {
			t.Fatalf("seed %d picked #%d, outside the pool %v", seed, got, pool)
		}
	}
}

func TestGenerateFilterRespectsDifficultyBand(t *testing.T) {
	byID := map[int64]Candidate{}
	for _, c := range bank() {
		byID[c.ID] = c
	}
	for seed := int64(0); seed < 50; seed++ {
		res, err := generatePaper([]TemplateItem{
			{Idx: 1, PickType: PickFilter, Score: 10,
				Filter: ProblemFilter{MinDifficulty: 2, MaxDifficulty: 3}},
		}, bank(), seed)
		if err != nil {
			t.Fatal(err)
		}
		d := byID[res.Items[0].ProblemID].Difficulty
		if d < 2 || d > 3 {
			t.Fatalf("seed %d picked difficulty %d, outside 2-3", seed, d)
		}
	}
}

func TestGenerateFilterTagModes(t *testing.T) {
	byID := map[int64]Candidate{}
	for _, c := range bank() {
		byID[c.ID] = c
	}

	t.Run("任一标签匹配", func(t *testing.T) {
		for seed := int64(0); seed < 30; seed++ {
			res, err := generatePaper([]TemplateItem{
				{Idx: 1, PickType: PickFilter, Score: 10,
					Filter: ProblemFilter{Tags: []string{"递归", "数组"}}},
			}, bank(), seed)
			if err != nil {
				t.Fatal(err)
			}
			c := byID[res.Items[0].ProblemID]
			if !c.hasTag("递归") && !c.hasTag("数组") {
				t.Fatalf("picked #%d with tags %v, matching neither tag", c.ID, c.Tags)
			}
		}
	})

	t.Run("全部标签匹配", func(t *testing.T) {
		res, err := generatePaper([]TemplateItem{
			{Idx: 1, PickType: PickFilter, Score: 10,
				Filter: ProblemFilter{Tags: []string{"循环", "数组"}, RequireAllTags: true}},
		}, bank(), 7)
		if err != nil {
			t.Fatal(err)
		}
		// Only #6 carries both tags.
		if res.Items[0].ProblemID != 6 {
			t.Fatalf("picked #%d, only #6 has both tags", res.Items[0].ProblemID)
		}
	})
}

func TestGenerateNeverRepeatsAProblem(t *testing.T) {
	// Five items drawn from an overlapping band; the paper must still contain
	// five distinct problems.
	items := []TemplateItem{}
	for i := 1; i <= 5; i++ {
		items = append(items, TemplateItem{
			Idx: i, PickType: PickFilter, Score: 10,
			Filter: ProblemFilter{MinDifficulty: 1, MaxDifficulty: 5},
		})
	}
	for seed := int64(0); seed < 50; seed++ {
		res, err := generatePaper(items, bank(), seed)
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		seen := map[int64]bool{}
		for _, id := range ids(res.Items) {
			if seen[id] {
				t.Fatalf("seed %d produced a paper with #%d twice: %v", seed, id, ids(res.Items))
			}
			seen[id] = true
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	items := []TemplateItem{
		{Idx: 1, PickType: PickFilter, Score: 10, Filter: ProblemFilter{MaxDifficulty: 2}},
		{Idx: 2, PickType: PickPool, Pool: []int64{3, 4, 5, 6}, Score: 10},
		{Idx: 3, PickType: PickFilter, Score: 10, Filter: ProblemFilter{MinDifficulty: 4}},
	}
	// A student who restarts their client must get the same paper back.
	first, err := generatePaper(items, bank(), 12345)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := generatePaper(items, bank(), 12345)
		if err != nil {
			t.Fatal(err)
		}
		a, b := ids(first.Items), ids(again.Items)
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("same seed produced different papers: %v vs %v", a, b)
			}
		}
	}
}

func TestPerStudentPapersDifferButStayComparable(t *testing.T) {
	// Difficulty is pinned per item, which is what makes per-student papers
	// gradeable against each other: everyone's question 2 is a level-3 problem.
	items := []TemplateItem{
		{Idx: 1, PickType: PickFilter, Score: 10, Filter: ProblemFilter{MinDifficulty: 1, MaxDifficulty: 1}},
		{Idx: 2, PickType: PickFilter, Score: 10, Filter: ProblemFilter{MinDifficulty: 3, MaxDifficulty: 3}},
	}
	byID := map[int64]Candidate{}
	for _, c := range bank() {
		byID[c.ID] = c
	}

	students := []string{"2023001", "2023002", "2023003", "2023004", "2023005", "2023006"}
	papers := map[string][]int64{}
	for _, no := range students {
		res, err := generatePaper(items, bank(), paperSeed(1, 42, no))
		if err != nil {
			t.Fatalf("%s: %v", no, err)
		}
		papers[no] = ids(res.Items)

		if d := byID[res.Items[0].ProblemID].Difficulty; d != 1 {
			t.Errorf("%s question 1 difficulty = %d, want 1", no, d)
		}
		if d := byID[res.Items[1].ProblemID].Difficulty; d != 3 {
			t.Errorf("%s question 2 difficulty = %d, want 3", no, d)
		}
	}

	// With two candidates per band, six students must not all draw alike.
	distinct := map[string]bool{}
	for _, p := range papers {
		distinct[fmt.Sprint(p)] = true
	}
	if len(distinct) < 2 {
		t.Errorf("all %d students drew the identical paper: %v", len(students), papers)
	}
}

func TestGenerateExcludesProblemsWithoutTestData(t *testing.T) {
	// #9 matches the filter on difficulty and tag but has no test data, so a
	// random draw must never land on it - a student cannot answer it.
	for seed := int64(0); seed < 100; seed++ {
		res, err := generatePaper([]TemplateItem{
			{Idx: 1, PickType: PickFilter, Score: 10,
				Filter: ProblemFilter{MinDifficulty: 2, MaxDifficulty: 2, Tags: []string{"分支"}}},
		}, bank(), seed)
		if err != nil {
			t.Fatal(err)
		}
		if res.Items[0].ProblemID == 9 {
			t.Fatalf("seed %d drew #9, which has no test data", seed)
		}
	}
}

func TestGenerateWarnsOnFixedProblemWithoutTestData(t *testing.T) {
	// A fixed pick is the teacher's explicit choice, so it is honoured - but
	// they must be told, because every student would get an unjudgeable problem.
	nine := int64(9)
	res, err := generatePaper([]TemplateItem{
		{Idx: 1, PickType: PickFixed, FixedProblemID: &nine, Score: 10},
	}, bank(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Items[0].ProblemID != 9 {
		t.Errorf("fixed pick not honoured: %+v", res.Items)
	}
	if len(res.Warnings) == 0 {
		t.Error("no warning for a fixed problem with no test data")
	}
}

func TestGenerateReportsImpossibleConstraints(t *testing.T) {
	tests := []struct {
		name string
		item TemplateItem
		want string
	}{
		{"难度区间无题", TemplateItem{Idx: 3, PickType: PickFilter,
			Filter: ProblemFilter{MinDifficulty: 9, MaxDifficulty: 9}}, "第 3 题"},
		{"标签无匹配", TemplateItem{Idx: 2, PickType: PickFilter,
			Filter: ProblemFilter{Tags: []string{"根本不存在的知识点"}}}, "第 2 题"},
		{"题目池全是无测试数据的题", TemplateItem{Idx: 1, PickType: PickPool,
			Pool: []int64{9}}, "第 1 题"},
		{"固定题不存在", TemplateItem{Idx: 4, PickType: PickFixed,
			FixedProblemID: func() *int64 { v := int64(999); return &v }()}, "不存在"},
		{"固定题未选择", TemplateItem{Idx: 5, PickType: PickFixed}, "没有选择题目"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := generatePaper([]TemplateItem{tc.item}, bank(), 1)
			if err == nil {
				t.Fatal("expected an error naming the offending item")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestRunningOutOfCandidatesIsAnError(t *testing.T) {
	// Three items all drawing from a two-problem band: the third cannot be
	// filled, and that must surface before the exam, not during it.
	items := []TemplateItem{}
	for i := 1; i <= 3; i++ {
		items = append(items, TemplateItem{Idx: i, PickType: PickFilter, Score: 10,
			Filter: ProblemFilter{MinDifficulty: 1, MaxDifficulty: 1}})
	}
	_, err := generatePaper(items, bank(), 1)
	if err == nil {
		t.Fatal("expected an error: only two level-1 problems exist")
	}
	if !strings.Contains(err.Error(), "第 3 题") {
		t.Errorf("error should name the item that could not be filled: %v", err)
	}
}

func TestTemplateRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p1 := seedProblem(t, s, "简单题", 2)
	p2 := seedProblem(t, s, "中等题", 2)

	tpl := &PaperTemplate{
		Name: "期末卷",
		Items: []TemplateItem{
			{PickType: PickFixed, FixedProblemID: &p1, Score: 30},
			{PickType: PickPool, Pool: []int64{p1, p2}, Score: 40},
			{PickType: PickFilter, Score: 30,
				Filter: ProblemFilter{MinDifficulty: 1, MaxDifficulty: 5, Tags: []string{"循环"}}},
		},
	}
	if err := s.SaveTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	if tpl.TotalScore != 100 {
		t.Errorf("total score = %d, want 100", tpl.TotalScore)
	}

	got, err := s.GetTemplate(ctx, tpl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(got.Items))
	}
	if got.Items[0].PickType != PickFixed || *got.Items[0].FixedProblemID != p1 {
		t.Errorf("fixed item lost: %+v", got.Items[0])
	}
	if len(got.Items[1].Pool) != 2 {
		t.Errorf("pool lost: %+v", got.Items[1])
	}
	if got.Items[2].Filter.Tags[0] != "循环" || got.Items[2].Filter.MaxDifficulty != 5 {
		t.Errorf("filter lost: %+v", got.Items[2].Filter)
	}
	if got.SeedNonce == 0 {
		t.Error("seed nonce not persisted; re-rolling would be impossible")
	}

	// Saving again must replace the items, not append to them.
	tpl.Items = tpl.Items[:1]
	if err := s.SaveTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTemplate(ctx, tpl.ID)
	if len(got.Items) != 1 {
		t.Errorf("re-save left %d items, want 1", len(got.Items))
	}
}

func TestRerollChangesTheDraw(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		seedProblem(t, s, "题", 1)
	}
	tpl := &PaperTemplate{Name: "随机卷", Items: []TemplateItem{
		{PickType: PickFilter, Score: 50, Filter: ProblemFilter{}},
		{PickType: PickFilter, Score: 50, Filter: ProblemFilter{}},
	}}
	if err := s.SaveTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}

	before, err := s.GeneratePaperFor(ctx, tpl.ID, 1, "2023001")
	if err != nil {
		t.Fatal(err)
	}
	// Without a nonce change the draw is fixed, which is what keeps a paper
	// stable for a student; re-rolling is the escape hatch.
	same, _ := s.GeneratePaperFor(ctx, tpl.ID, 1, "2023001")
	if ids(before.Items)[0] != ids(same.Items)[0] {
		t.Fatal("generation is not stable between calls")
	}

	changed := false
	for i := 0; i < 10 && !changed; i++ {
		if err := s.RerollTemplate(ctx, tpl.ID); err != nil {
			t.Fatal(err)
		}
		after, err := s.GeneratePaperFor(ctx, tpl.ID, 1, "2023001")
		if err != nil {
			t.Fatal(err)
		}
		a, b := ids(before.Items), ids(after.Items)
		if a[0] != b[0] || a[1] != b[1] {
			changed = true
		}
	}
	if !changed {
		t.Error("re-rolling never changed the draw")
	}
}
