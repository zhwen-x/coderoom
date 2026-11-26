package server

import (
	"net/http"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

// Replaying how an answer was written (M5).
//
// This is the other half of the bargain made back in M0: students compose in
// the lab's own IDE and paste the result in, so the clipboard cannot be
// blocked and "no pasting" was never available as a rule. What replaced it was
// keeping every timed snapshot. Until now those were only ever counted; here
// they are played back, which is what makes the trade actually pay.
//
// The question a teacher is answering is not "did they paste" - everyone
// pastes, that is the designed workflow - but "did this answer arrive in one
// piece". An answer typed over twenty minutes grows a few hundred bytes at a
// time. One that appeared whole between two snapshots did not get written in
// this room.

type replayStep struct {
	SubmissionID int64  `json:"submission_id"`
	At           int64  `json:"at"`
	Bytes        int    `json:"bytes"`
	Kind         string `json:"kind"`
	Code         string `json:"code"`
	// Added is the change in size from the previous step, which is what the
	// chart draws and what makes a single arrival obvious.
	Added int `json:"added"`
}

type replayEvent struct {
	At      int64  `json:"at"`
	Type    string `json:"type"`
	Chars   int    `json:"chars"`
	Lines   int    `json:"lines"`
	Problem int64  `json:"problem_id"`
}

type replaySummary struct {
	Steps      int   `json:"steps"`
	FinalBytes int   `json:"final_bytes"`
	SpanMS     int64 `json:"span_ms"`
	// LargestJump is the biggest single growth between consecutive snapshots,
	// and LargestJumpPct the share of the finished answer that arrived in it.
	// A high percentage is the signal; everything else here is context for it.
	LargestJump    int   `json:"largest_jump"`
	LargestJumpPct int   `json:"largest_jump_pct"`
	LargestJumpAt  int64 `json:"largest_jump_at"`
	PasteCount     int   `json:"paste_count"`
	PasteChars     int   `json:"paste_chars"`
}

type replayResponse struct {
	SubmissionID int64         `json:"submission_id"`
	StudentNo    string        `json:"student_no"`
	Name         string        `json:"name"`
	ProblemID    int64         `json:"problem_id"`
	ProblemTitle string        `json:"problem_title"`
	Language     string        `json:"language"`
	Summary      replaySummary `json:"summary"`
	Steps        []replayStep  `json:"steps"`
	Events       []replayEvent `json:"events"`
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "提交 ID 无效")
		return
	}
	sub, err := s.st.GetSubmission(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "提交不存在")
		return
	}
	timeline, err := s.st.SnapshotTimeline(r.Context(), sub.SessionID, sub.ProblemID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	out := replayResponse{
		SubmissionID: sub.ID,
		StudentNo:    sub.StudentNo,
		ProblemID:    sub.ProblemID,
		Language:     sub.Language,
		Steps:        []replayStep{},
		Events:       []replayEvent{},
	}
	if titles, err := s.st.ProblemTitles(r.Context()); err == nil {
		out.ProblemTitle = titles[sub.ProblemID]
	}

	prevBytes := 0
	for _, row := range timeline {
		step := replayStep{
			SubmissionID: row.ID, At: row.CreatedAt,
			Bytes: row.CodeBytes, Kind: row.Kind,
			Added: row.CodeBytes - prevBytes,
		}
		out.Name = row.Name
		// Reading every version is what makes this a replay rather than a
		// chart. They are small, and there are at most a couple of hundred.
		if full, err := s.st.GetSubmission(r.Context(), row.ID); err == nil {
			if code, err := s.st.ReadSubmissionCode(full); err == nil {
				step.Code = code
			}
		}
		out.Steps = append(out.Steps, step)
		prevBytes = row.CodeBytes
	}

	out.Events = s.pasteEventsFor(r, sub)
	out.Summary = summariseReplay(out.Steps, out.Events)
	writeJSON(w, http.StatusOK, out)
}

// pasteEventsFor collects this student's paste activity for this problem, so
// the timeline shows what arrived at the same moment the code grew.
func (s *Server) pasteEventsFor(r *http.Request, sub *store.Submission) []replayEvent {
	rows, err := s.st.QueryEvents(r.Context(), store.EventQuery{
		ExamID: sub.ExamID, StudentNo: sub.StudentNo, Limit: 1000, Ascending: true,
	})
	if err != nil {
		return []replayEvent{}
	}
	out := []replayEvent{}
	for _, ev := range rows {
		if ev.Type != protocol.EvPaste && ev.Type != protocol.EvPasteLarge {
			continue
		}
		e := replayEvent{At: ev.ServerTime, Type: ev.Type}
		if v, ok := ev.Payload["chars"].(float64); ok {
			e.Chars = int(v)
		}
		if v, ok := ev.Payload["lines"].(float64); ok {
			e.Lines = int(v)
		}
		if v, ok := ev.Payload["problem_id"].(float64); ok {
			e.Problem = int64(v)
		}
		// A paste recorded against a different problem belongs to that one's
		// timeline, not this one.
		if e.Problem != 0 && e.Problem != sub.ProblemID {
			continue
		}
		out = append(out, e)
	}
	return out
}

func summariseReplay(steps []replayStep, events []replayEvent) replaySummary {
	sum := replaySummary{Steps: len(steps)}
	if len(steps) == 0 {
		return sum
	}
	sum.FinalBytes = steps[len(steps)-1].Bytes
	sum.SpanMS = steps[len(steps)-1].At - steps[0].At

	for _, st := range steps {
		if st.Added > sum.LargestJump {
			sum.LargestJump = st.Added
			sum.LargestJumpAt = st.At
		}
	}
	if sum.FinalBytes > 0 {
		sum.LargestJumpPct = sum.LargestJump * 100 / sum.FinalBytes
		if sum.LargestJumpPct > 100 {
			sum.LargestJumpPct = 100
		}
	}
	for _, ev := range events {
		sum.PasteCount++
		sum.PasteChars += ev.Chars
	}
	return sum
}
