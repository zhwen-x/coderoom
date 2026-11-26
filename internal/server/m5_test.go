package server

import (
	"net/http"
	"testing"
	"time"

	"coderoom/internal/protocol"
	"coderoom/internal/similarity"
	"coderoom/internal/store"
)

// seedProblem creates a problem with the given extra fields and one test case.
func (h *harness) seedProblem(t *testing.T, fields map[string]any,
	cases []map[string]any) int64 {

	t.Helper()
	body := map[string]any{
		"title": "题", "time_limit_ms": 2000, "memory_limit_mb": 256,
		"languages": []string{"cpp", "python"},
	}
	for k, v := range fields {
		body[k] = v
	}
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", body, true)
	h.mustJSON(status, raw, &p)

	status, raw = h.do(http.MethodPut,
		"/api/teacher/problems/"+itoa(p.ID)+"/testcases", cases, true)
	h.mustJSON(status, raw, nil)
	return p.ID
}

// The tolerance has to survive the round trip, because a problem author sets
// it once and then never looks at it again.
func TestFloatToleranceIsStoredAndReturned(t *testing.T) {
	h := newHarness(t)
	id := h.seedProblem(t, map[string]any{"float_eps": 1e-6},
		[]map[string]any{{"input": "1\n", "output": "1.0\n", "score": 10}})

	var got store.Problem
	status, raw := h.do(http.MethodGet, "/api/teacher/problems/"+itoa(id), nil, true)
	h.mustJSON(status, raw, &got)
	if got.FloatEps != 1e-6 {
		t.Errorf("float_eps = %v，应为 1e-6", got.FloatEps)
	}

	// A nonsense tolerance would accept anything at all, so it is refused
	// rather than stored.
	bad := h.seedProblem(t, map[string]any{"float_eps": 500.0},
		[]map[string]any{{"input": "1\n", "output": "1.0\n", "score": 10}})
	status, raw = h.do(http.MethodGet, "/api/teacher/problems/"+itoa(bad), nil, true)
	h.mustJSON(status, raw, &got)
	if got.FloatEps != 0 {
		t.Errorf("荒谬的容差 %v 被接受了，应当归零", got.FloatEps)
	}
}

// The end of the chain: a real submission whose answer differs in the last
// digit has to be accepted when the problem asks for a tolerance, and rejected
// when it does not. Everything between the problem editor and the comparator
// is exercised here.
func TestFloatToleranceChangesTheVerdict(t *testing.T) {
	h := newHarness(t)
	if !h.srv.judger.Toolchain().Available()[protocol.LangCPP] {
		t.Skip("本机没有 C++ 工具链")
	}
	h.startWorkers()

	// Expected answer has six decimals; the program prints ten.
	cases := []map[string]any{{"input": "2\n", "output": "1.414214\n", "score": 10}}
	tolerant := h.seedProblem(t, map[string]any{"float_eps": 1e-6}, cases)
	strict := h.seedProblem(t, map[string]any{"float_eps": 0}, cases)

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "浮点练习", "type": "practice", "state": "published",
		"show_results": true, "allow_rejudge": true,
		"items": []map[string]any{
			{"problem_id": tolerant, "score": 10},
			{"problem_id": strict, "score": 10},
		},
	}, true)
	h.mustJSON(status, raw, &e)
	h.login(e.ID, "2023", "张三", "")

	const code = `#include <cstdio>
#include <cmath>
int main(){ double x; scanf("%lf", &x); printf("%.10f\n", sqrt(x)); return 0; }`

	verdictFor := func(problemID int64) string {
		t.Helper()
		status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
			ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
		}, false)
		h.mustJSON(status, raw, nil)

		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			var res protocol.ResultsResponse
			status, raw := h.do(http.MethodGet, "/api/client/results", nil, false)
			h.mustJSON(status, raw, &res)
			for _, r := range res.Results {
				if r.ProblemID == problemID && r.Verdict != protocol.VerdictPending {
					return r.Verdict
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("题目 %d 的判题结果一直没有回来", problemID)
		return ""
	}

	if v := verdictFor(tolerant); v != protocol.VerdictAC {
		t.Errorf("设了容差仍然判 %s —— 正确答案被拒，整道题全班都会错", v)
	}
	if v := verdictFor(strict); v != protocol.VerdictWA {
		t.Errorf("未设容差却判 %s —— 默认必须是严格比对", v)
	}
}

// -------------------------------------------------------------- 查重

// seedSimilarityExam puts three students' answers to one problem on disk: two
// that are the same program with the names changed, and one written
// independently.
func (h *harness) seedSimilarityExam(t *testing.T) (examID, problemID int64) {
	t.Helper()
	problemID = h.seedProblem(t, nil,
		[]map[string]any{{"input": "1\n", "output": "1\n", "score": 10}})

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "查重测试", "type": "exam", "state": "running",
		"items": []map[string]any{{"problem_id": problemID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	for _, who := range []struct {
		no, name, code string
	}{
		{"2001", "甲", longCPP("alpha")},
		{"2002", "乙", longCPP("beta")},
		{"2003", "丙", differentCPP},
	} {
		h.login(e.ID, who.no, who.name, "")
		status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
			ProblemID: problemID, Language: "cpp", Code: who.code,
			Hash: store.HashCode(who.code),
		}, false)
		h.mustJSON(status, raw, nil)
	}
	return e.ID, problemID
}

func TestSimilarityReportFindsTheCopyAndNotTheRest(t *testing.T) {
	h := newHarness(t)
	examID, problemID := h.seedSimilarityExam(t)

	var rep simReport
	status, raw := h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(examID)+"/similarity?min=40", nil, true)
	h.mustJSON(status, raw, &rep)

	if len(rep.Problems) != 1 || rep.Problems[0].ProblemID != problemID {
		t.Fatalf("报告的题目不对: %+v", rep.Problems)
	}
	p := rep.Problems[0]
	if p.Compared != 3 {
		t.Fatalf("参与比较的提交数为 %d，应为 3（跳过: %v）", p.Compared, p.Skipped)
	}
	if len(p.Pairs) != 1 {
		t.Fatalf("应当只有一对相似，得到 %d 对: %+v", len(p.Pairs), p.Pairs)
	}
	pair := p.Pairs[0]
	got := map[string]bool{pair.A.StudentNo: true, pair.B.StudentNo: true}
	if !got["2001"] || !got["2002"] {
		t.Errorf("揪出来的是 %s 与 %s，应当是 2001 与 2002", pair.A.StudentNo, pair.B.StudentNo)
	}
	if pair.A.Name == "" || pair.B.Name == "" {
		t.Error("报告里没有学生姓名，教师无法直接认人")
	}
	if pair.Shared == 0 {
		t.Error("共享指纹数为 0，教师无法判断这个比例有多少分量")
	}
	if pair.A.SubmissionID == 0 || pair.B.SubmissionID == 0 {
		t.Error("没有提交 ID，无法并排打开两份代码")
	}
}

// The cutoff is the teacher's dial, and what it does is decide how much noise
// to let in. Renaming every variable leaves the token stream untouched, so the
// copied pair scores 100 and survives any threshold; the two unrelated pairs
// only appear once the dial is turned right down.
func TestSimilarityCutoffFilters(t *testing.T) {
	h := newHarness(t)
	examID, _ := h.seedSimilarityExam(t)

	pairsAt := func(min string) []simPair {
		t.Helper()
		var rep simReport
		status, raw := h.do(http.MethodGet,
			"/api/teacher/exams/"+itoa(examID)+"/similarity?min="+min, nil, true)
		h.mustJSON(status, raw, &rep)
		return rep.Problems[0].Pairs
	}

	strict := pairsAt("99")
	if len(strict) != 1 {
		t.Errorf("阈值 99%% 应当只剩那对照抄的，得到 %d 对: %+v", len(strict), strict)
	} else if strict[0].Score != 100 {
		t.Errorf("改名照抄的相似度为 %d%%，token 流未变应为 100%%", strict[0].Score)
	}

	if n := len(pairsAt("0")); n != 3 {
		t.Errorf("阈值 0 应返回全部 3 对，得到 %d", n)
	}
}

// A problem with one obvious answer makes the whole class look alike. Saying
// "skipped, too short" is the only honest report; listing fifty pairs would
// train the teacher to close the page.
func TestSimilaritySkipsSubmissionsTooShortToJudge(t *testing.T) {
	h := newHarness(t)
	problemID := h.seedProblem(t, nil,
		[]map[string]any{{"input": "1 2\n", "output": "3\n", "score": 10}})

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "A+B", "type": "exam", "state": "running",
		"items": []map[string]any{{"problem_id": problemID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	const tiny = "#include <iostream>\nint main(){int a,b;std::cin>>a>>b;std::cout<<a+b;}"
	for _, no := range []string{"3001", "3002"} {
		h.login(e.ID, no, "学生"+no, "")
		status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
			ProblemID: problemID, Language: "cpp", Code: tiny, Hash: store.HashCode(tiny),
		}, false)
		h.mustJSON(status, raw, nil)
	}

	var rep simReport
	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(e.ID)+"/similarity", nil, true)
	h.mustJSON(status, raw, &rep)

	p := rep.Problems[0]
	if len(p.Pairs) != 0 {
		t.Errorf("两份一模一样的 A+B 被报成了 %d 对相似，这是必然的误报", len(p.Pairs))
	}
	if len(p.Skipped) != 2 {
		t.Errorf("应当明确说明跳过了 2 份过短提交，得到 %v", p.Skipped)
	}
	if rep.MinTokens != similarity.MinTokens {
		t.Error("报告没有告诉前端「过短」的判定标准")
	}
}

// Practice allows resubmission, so a student can have a dozen finals for one
// problem. Comparing them all would report everyone against their own drafts
// at 100% and bury the pairs that matter.
func TestSimilarityComparesOnlyTheLatestAttempt(t *testing.T) {
	h := newHarness(t)
	problemID := h.seedProblem(t, nil,
		[]map[string]any{{"input": "1\n", "output": "1\n", "score": 10}})

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "可重提练习", "type": "practice", "state": "published",
		"show_results": true, "allow_rejudge": true,
		"items": []map[string]any{{"problem_id": problemID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)

	h.login(e.ID, "4001", "重提同学", "")
	for _, code := range []string{longCPP("one"), longCPP("two"), longCPP("three")} {
		status, raw := h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
			ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
		}, false)
		h.mustJSON(status, raw, nil)
	}

	var rep simReport
	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(e.ID)+"/similarity?min=0", nil, true)
	h.mustJSON(status, raw, &rep)

	p := rep.Problems[0]
	if p.Compared != 1 {
		t.Errorf("同一学生的 3 次提交参与了 %d 份比较，应当只留最后一次", p.Compared)
	}
	if len(p.Pairs) != 0 {
		t.Errorf("学生被拿来和自己比了: %+v", p.Pairs)
	}
}

// ------------------------------------------------------------ 过程回放

func TestReplayShowsHowTheAnswerGrew(t *testing.T) {
	h := newHarness(t)
	problemID := h.seedProblem(t, nil,
		[]map[string]any{{"input": "1\n", "output": "1\n", "score": 10}})

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "回放", "type": "exam", "state": "running",
		"items": []map[string]any{{"problem_id": problemID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)
	h.login(e.ID, "5001", "过程同学", "")

	// Three snapshots that grow a little, then a final that arrives whole -
	// the shape that makes a teacher look twice.
	full := longCPP("x")
	grow := []string{
		full[:len(full)/4],
		full[:len(full)/2],
		full[:len(full)*3/4],
	}
	for _, code := range grow {
		status, raw := h.do(http.MethodPost, "/api/client/snapshot", protocol.SnapshotRequest{
			Items: []protocol.SnapshotItem{{
				ProblemID: problemID, Language: "cpp", Code: code, Hash: store.HashCode(code),
			}},
		}, false)
		h.mustJSON(status, raw, nil)
	}
	status, raw = h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{{
			Type: protocol.EvPasteLarge, Severity: protocol.SevWarn,
			Payload: map[string]any{"chars": 900, "lines": 40, "problem_id": problemID},
		}},
	}, false)
	h.mustJSON(status, raw, nil)

	var sub protocol.SubmitResponse
	status, raw = h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: problemID, Language: "cpp", Code: full, Hash: store.HashCode(full),
	}, false)
	h.mustJSON(status, raw, &sub)

	var rep replayResponse
	status, raw = h.do(http.MethodGet,
		"/api/teacher/submissions/"+itoa(sub.SubmissionID)+"/replay", nil, true)
	h.mustJSON(status, raw, &rep)

	if rep.StudentNo != "5001" || rep.Name != "过程同学" {
		t.Errorf("回放没带上学生身份: %+v", rep)
	}
	if len(rep.Steps) != 4 {
		t.Fatalf("应当有 4 个版本（3 次快照 + 1 次终稿），得到 %d", len(rep.Steps))
	}
	if rep.Steps[0].Code == "" || rep.Steps[3].Code == "" {
		t.Error("回放缺少各版本的代码，无法真正回放")
	}
	if rep.Steps[0].Added != rep.Steps[0].Bytes {
		t.Errorf("第一个版本的增量应等于其大小，得到 %d", rep.Steps[0].Added)
	}
	if rep.Summary.FinalBytes != len(full) {
		t.Errorf("终稿大小 %d，应为 %d", rep.Summary.FinalBytes, len(full))
	}
	if rep.Summary.LargestJump <= 0 || rep.Summary.LargestJumpPct <= 0 {
		t.Errorf("没有算出最大单次增量: %+v", rep.Summary)
	}
	if rep.Summary.PasteCount != 1 || rep.Summary.PasteChars != 900 {
		t.Errorf("粘贴事件没有并进时间线: %+v", rep.Summary)
	}
}

// A paste recorded against another problem belongs to that problem's timeline.
func TestReplayIgnoresPastesForOtherProblems(t *testing.T) {
	h := newHarness(t)
	mine := h.seedProblem(t, nil,
		[]map[string]any{{"input": "1\n", "output": "1\n", "score": 10}})
	other := h.seedProblem(t, nil,
		[]map[string]any{{"input": "1\n", "output": "1\n", "score": 10}})

	var e store.Exam
	status, raw := h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "两题", "type": "exam", "state": "running",
		"items": []map[string]any{
			{"problem_id": mine, "score": 50}, {"problem_id": other, "score": 50}},
	}, true)
	h.mustJSON(status, raw, &e)
	h.login(e.ID, "6001", "两题同学", "")

	status, raw = h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{
			{Type: protocol.EvPasteLarge, Severity: protocol.SevWarn,
				Payload: map[string]any{"chars": 100, "problem_id": mine}},
			{Type: protocol.EvPasteLarge, Severity: protocol.SevWarn,
				Payload: map[string]any{"chars": 999, "problem_id": other}},
		},
	}, false)
	h.mustJSON(status, raw, nil)

	code := longCPP("y")
	var sub protocol.SubmitResponse
	status, raw = h.do(http.MethodPost, "/api/client/submit", protocol.SubmitRequest{
		ProblemID: mine, Language: "cpp", Code: code, Hash: store.HashCode(code),
	}, false)
	h.mustJSON(status, raw, &sub)

	var rep replayResponse
	status, raw = h.do(http.MethodGet,
		"/api/teacher/submissions/"+itoa(sub.SubmissionID)+"/replay", nil, true)
	h.mustJSON(status, raw, &rep)

	if rep.Summary.PasteCount != 1 || rep.Summary.PasteChars != 100 {
		t.Errorf("串进了另一题的粘贴事件: %+v", rep.Summary)
	}
}

// ------------------------------------------------------------------ fixtures

// longCPP is a submission long enough to be fingerprinted, parameterised by
// the identifier names so two copies differ only in spelling.
func longCPP(name string) string {
	return `#include <iostream>
using namespace std;
int main() {
    int ` + name + `_n;
    cin >> ` + name + `_n;
    int ` + name + `_a[1005];
    for (int ` + name + `_i = 0; ` + name + `_i < ` + name + `_n; ` + name + `_i++) {
        cin >> ` + name + `_a[` + name + `_i];
    }
    for (int ` + name + `_i = 0; ` + name + `_i < ` + name + `_n - 1; ` + name + `_i++) {
        for (int ` + name + `_j = 0; ` + name + `_j < ` + name + `_n - 1 - ` + name + `_i; ` + name + `_j++) {
            if (` + name + `_a[` + name + `_j] > ` + name + `_a[` + name + `_j + 1]) {
                int ` + name + `_t = ` + name + `_a[` + name + `_j];
                ` + name + `_a[` + name + `_j] = ` + name + `_a[` + name + `_j + 1];
                ` + name + `_a[` + name + `_j + 1] = ` + name + `_t;
            }
        }
    }
    for (int ` + name + `_i = 0; ` + name + `_i < ` + name + `_n; ` + name + `_i++) {
        cout << ` + name + `_a[` + name + `_i] << " ";
    }
    cout << endl;
    return 0;
}`
}

const differentCPP = `#include <cstdio>
#include <algorithm>
#include <vector>
int main() {
    int count;
    scanf("%d", &count);
    std::vector<int> values;
    for (int idx = 0; idx < count; idx++) {
        int tmp;
        scanf("%d", &tmp);
        values.push_back(tmp);
    }
    std::sort(values.begin(), values.end());
    for (size_t pos = 0; pos < values.size(); pos++) {
        printf("%d ", values[pos]);
    }
    printf("\n");
    return 0;
}`
