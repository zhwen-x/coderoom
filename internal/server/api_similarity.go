package server

import (
	"net/http"
	"sort"
	"strconv"

	"coderoom/internal/protocol"
	"coderoom/internal/similarity"
	"coderoom/internal/store"
)

// Similarity reporting (M5). Every final submission in an exam is fingerprinted
// and compared with every other answer to the same problem.
//
// Computed on request rather than stored. At the scale this system targets -
// fifty students over a handful of problems - the whole comparison is a few
// hundred small files and a few thousand pairs, which takes milliseconds; a
// cached table would only add a way for the report to disagree with the
// submissions it describes after a re-judge or a promoted snapshot.
//
// The report deliberately says "相似" and never "抄袭". It cannot tell copying
// from two people arriving at the same answer, and on a problem with one
// obvious solution the whole class will look alike. What it can do is put the
// handful of pairs worth a teacher's attention at the top of a list.

const (
	defaultSimilarityCutoff = 50
	// maxPairsPerProblem bounds the response when a whole class really does
	// look alike - usually a sign the problem was too constrained, not that
	// fifty people cheated.
	maxPairsPerProblem = 100
)

type simStudent struct {
	StudentNo    string `json:"student_no"`
	Name         string `json:"name"`
	SubmissionID int64  `json:"submission_id"`
	Tokens       int    `json:"tokens"`
}

type simPair struct {
	A      simStudent `json:"a"`
	B      simStudent `json:"b"`
	Score  int        `json:"score"`
	Shared int        `json:"shared"`
}

type simProblem struct {
	ProblemID int64     `json:"problem_id"`
	Title     string    `json:"title"`
	Compared  int       `json:"compared"`
	Skipped   []string  `json:"skipped"`
	Pairs     []simPair `json:"pairs"`
}

type simReport struct {
	ExamID    int64        `json:"exam_id"`
	Cutoff    int          `json:"cutoff"`
	MinTokens int          `json:"min_tokens"`
	Problems  []simProblem `json:"problems"`
}

func (s *Server) handleSimilarity(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	cutoff := defaultSimilarityCutoff
	if v, err := strconv.Atoi(r.URL.Query().Get("min")); err == nil && v >= 0 && v <= 100 {
		cutoff = v
	}

	subs, err := s.st.ListSubmissions(r.Context(), id, protocol.KindFinal, 2000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	names, err := s.studentNames(r, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	titles, err := s.st.ProblemTitles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	report := simReport{
		ExamID: id, Cutoff: cutoff,
		MinTokens: similarity.MinTokens,
		Problems:  []simProblem{},
	}
	for _, g := range groupLatestFinals(subs) {
		p := simProblem{
			ProblemID: g.problemID,
			Title:     titles[g.problemID],
			Skipped:   []string{},
			Pairs:     []simPair{},
		}

		docs := make([]similarity.Doc, 0, len(g.subs))
		owners := make([]simStudent, 0, len(g.subs))
		for _, sub := range g.subs {
			code, err := s.st.ReadSubmissionCode(sub)
			if err != nil {
				continue // the submission row outlived its file; nothing to compare
			}
			d := similarity.Analyse(sub.StudentNo, sub.Language, code)
			who := simStudent{
				StudentNo: sub.StudentNo, Name: names[sub.StudentNo],
				SubmissionID: sub.ID, Tokens: d.Tokens,
			}
			if d.TooShort {
				p.Skipped = append(p.Skipped, sub.StudentNo)
				continue
			}
			docs = append(docs, d)
			owners = append(owners, who)
		}
		p.Compared = len(docs)
		sort.Strings(p.Skipped)

		for i := 0; i < len(docs); i++ {
			for j := i + 1; j < len(docs); j++ {
				m := similarity.Compare(docs[i], docs[j])
				if m.Score < cutoff {
					continue
				}
				p.Pairs = append(p.Pairs, simPair{
					A: owners[i], B: owners[j], Score: m.Score, Shared: m.Shared,
				})
			}
		}
		sort.SliceStable(p.Pairs, func(a, b int) bool {
			if p.Pairs[a].Score != p.Pairs[b].Score {
				return p.Pairs[a].Score > p.Pairs[b].Score
			}
			return p.Pairs[a].Shared > p.Pairs[b].Shared
		})
		if len(p.Pairs) > maxPairsPerProblem {
			p.Pairs = p.Pairs[:maxPairsPerProblem]
		}
		report.Problems = append(report.Problems, p)
	}
	sort.Slice(report.Problems, func(a, b int) bool {
		return report.Problems[a].ProblemID < report.Problems[b].ProblemID
	})
	writeJSON(w, http.StatusOK, report)
}

type problemGroup struct {
	problemID int64
	subs      []*store.Submission
}

// groupLatestFinals keeps one submission per student per problem - the newest.
//
// Practice sessions allow resubmission, so a student can have a dozen finals
// for one problem. Comparing all of them would report every student against
// their own earlier drafts at 100%, burying the pairs that matter.
func groupLatestFinals(subs []*store.Submission) []problemGroup {
	type key struct {
		problem int64
		student string
	}
	latest := map[key]*store.Submission{}
	for _, sub := range subs {
		k := key{sub.ProblemID, sub.StudentNo}
		if prev, ok := latest[k]; !ok || sub.ID > prev.ID {
			latest[k] = sub
		}
	}

	byProblem := map[int64][]*store.Submission{}
	for k, sub := range latest {
		byProblem[k.problem] = append(byProblem[k.problem], sub)
	}
	out := make([]problemGroup, 0, len(byProblem))
	for pid, list := range byProblem {
		// Stable order so the report does not reshuffle between refreshes.
		sort.Slice(list, func(i, j int) bool { return list[i].StudentNo < list[j].StudentNo })
		out = append(out, problemGroup{problemID: pid, subs: list})
	}
	return out
}

// studentNames maps student numbers to names for this exam's sessions.
func (s *Server) studentNames(r *http.Request, examID int64) (map[string]string, error) {
	list, err := s.st.ListSessions(r.Context(), examID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, sess := range list {
		out[sess.StudentNo] = sess.Name
	}
	return out, nil
}
