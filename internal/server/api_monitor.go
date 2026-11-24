package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

// The invigilation side of the server (M3): the control policy the teacher
// edits, the dashboard's live feed, and the one alert a client cannot raise
// for itself.

// A client that has not been heard from for this long is treated as gone. It
// has to be comfortably longer than the heartbeat interval (10s by default) so
// a single dropped packet on a busy lab switch does not read as a student
// pulling the network cable.
const heartbeatGraceMS = 45_000

// offlineSweepInterval is how often the server looks for clients that have
// stopped reporting.
const offlineSweepInterval = 10 * time.Second

// sweepOfflineSessions raises the one event no client can send: its own death.
//
// Everything else in the event log is self-reported, which means a student who
// kills the client process goes quiet - and quiet is exactly what a well
// behaved student also looks like. Watching for absence from this side is what
// closes that gap.
func (s *Server) sweepOfflineSessions(ctx context.Context) {
	ticker := time.NewTicker(offlineSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			list, err := s.st.ClaimSilentSessions(ctx, store.NowMS()-heartbeatGraceMS)
			if err != nil {
				s.logger.Printf("心跳巡检: %v", err)
				continue
			}
			for _, sess := range list {
				s.logEventCtx(ctx, sess, protocol.Event{
					Type:     protocol.EvHeartbeatLost,
					Severity: protocol.SevAlert,
					Payload: map[string]any{
						"silent_ms": store.NowMS() - sess.LastHeartbeatAt,
						"machine":   sess.MachineName,
					},
				})
				s.logger.Printf("告警: %s(%s) 客户端失联，机器 %s",
					sess.Name, sess.StudentNo, sess.MachineName)
			}
		}
	}
}

// ------------------------------------------------------------------- policy

func (s *Server) handleSetExamPolicy(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	var p protocol.Policy
	if err := readJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "请求格式错误")
		return
	}
	p = sanitizePolicy(p)

	version, err := s.st.SetExamPolicy(r.Context(), id, p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	p.Version = version
	s.audit(r, "exam.policy", fmt.Sprint(id), map[string]any{
		"version": version, "monitors": monitorSummary(p),
	})
	writeJSON(w, http.StatusOK, p)
}

// sanitizePolicy bounds what a teacher can save.
//
// The enforcement flags are honoured from M4 on. AllowedHosts is still cleared
// because nothing reads it: the client permits the whole local network by
// construction, which already covers the judge server and the classroom
// software, and a list that looks like it grants exceptions while granting
// none is worse than no list.
func sanitizePolicy(p protocol.Policy) protocol.Policy {
	p.AllowedHosts = nil

	p.SnapshotIntervalSec = clampInt(p.SnapshotIntervalSec, 10, 600, 30)
	p.HeartbeatIntervalSec = clampInt(p.HeartbeatIntervalSec, 5, 120, 10)
	p.ProcessBlacklist = cleanNames(p.ProcessBlacklist)
	p.ProcessWhitelist = cleanNames(p.ProcessWhitelist)
	return p
}

func clampInt(v, lo, hi, fallback int) int {
	if v == 0 {
		return fallback
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// cleanNames trims, de-duplicates and sorts a process list. Sorting is for the
// teacher's benefit: the list is edited as text in a textarea, and a stable
// order means a save does not reshuffle what they are reading.
func cleanNames(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, raw := range in {
		n := strings.TrimSpace(raw)
		if n == "" || len(n) > 128 {
			continue
		}
		key := strings.ToLower(strings.TrimSuffix(n, ".exe"))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	if len(out) == 0 {
		return nil
	}
	return out
}

func monitorSummary(p protocol.Policy) []string {
	var out []string
	for _, c := range []struct {
		on   bool
		name string
	}{
		{p.MonitorUSB, "usb"}, {p.MonitorProcess, "process"},
		{p.MonitorForeground, "foreground"}, {p.MonitorPaste, "paste"},
		{p.MonitorNetwork, "network"},
		{p.BlockUSBStorage, "block_usb"}, {p.BlockInternet, "block_internet"},
	} {
		if c.on {
			out = append(out, c.name)
		}
	}
	return out
}

// handleReleaseGuard is the emergency escape: drop every lock in this exam,
// now.
//
// It works by turning the enforcement flags off and bumping the policy
// version, which is the same path every other policy change takes - so there
// is no second mechanism that might be the broken one on the day it is needed.
// Every client unlocks on its next heartbeat, including machines that were
// asleep or rebooting when the button was pressed.
//
// For a single machine whose client is gone, the escape is local and needs no
// server: coderoom-client.exe -restore.
func (s *Server) handleReleaseGuard(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	exam, err := s.st.GetExam(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "考试不存在")
		return
	}
	p := exam.Policy
	p.BlockUSBStorage = false
	p.BlockInternet = false
	version, err := s.st.SetExamPolicy(r.Context(), id, sanitizePolicy(p))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	s.audit(r, "exam.release_guard", fmt.Sprint(id), map[string]any{"version": version})
	s.logger.Printf("已对考试 %d 下发全局解除管控（策略 v%d）", id, version)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "policy_version": version,
		"message": "已解除；各学生机将在下一次心跳（默认 10 秒内）自动恢复 U 盘与网络。",
	})
}

// ---------------------------------------------------------------- dashboard

// sessionView adds the derived invigilation state the dashboard needs.
type sessionView struct {
	*store.Session
	Online      bool             `json:"online"`
	SilentForMS int64            `json:"silent_for_ms"`
	Events      store.EventTally `json:"events"`
}

func (s *Server) buildSessionViews(ctx context.Context, examID int64) ([]sessionView, error) {
	list, err := s.st.ListSessions(ctx, examID)
	if err != nil {
		return nil, err
	}
	tally, err := s.st.EventCountsByStudent(ctx, examID)
	if err != nil {
		return nil, err
	}
	now := store.NowMS()
	out := []sessionView{}
	for _, x := range list {
		silent := now - x.LastHeartbeatAt
		out = append(out, sessionView{
			Session:     x,
			Online:      silent <= heartbeatGraceMS,
			SilentForMS: silent,
			Events:      tally[x.StudentNo],
		})
	}
	return out, nil
}

func (s *Server) handleExamSessions(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	out, err := s.buildSessionViews(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleExamEvents(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	q := store.EventQuery{ExamID: id, StudentNo: r.URL.Query().Get("student")}
	q.AfterID, _ = strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	q.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if sev := r.URL.Query().Get("severity"); sev != "" {
		q.Severities = strings.Split(sev, ",")
	}
	rows, err := s.st.QueryEvents(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// ---------------------------------------------------------------- live feed

const (
	streamEventTick   = 1 * time.Second
	streamSessionTick = 3 * time.Second
	streamKeepalive   = 15 * time.Second
)

// handleExamStream is the dashboard's live feed, as Server-Sent Events.
//
// It polls the database and pushes the deltas, rather than having logEvent
// publish into an in-memory hub. The hub would shave a second off the latency
// and cost rather more than that is worth: the cursor in this loop is the
// single source of truth, so a teacher who reloads the page, loses the Wi-Fi
// for a minute or reconnects after a server restart resumes exactly where they
// stopped, with nothing buffered anywhere and nothing to miss. A second of
// delay does not matter to someone who then has to walk across the room.
//
// What SSE buys over the polling the dashboard used to do is not freshness but
// stability: new events arrive as a delta and are prepended, so the table the
// teacher is reading stops being rebuilt from scratch underneath them.
func (s *Server) handleExamStream(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "考试 ID 无效")
		return
	}
	if _, err := s.st.GetExam(r.Context(), id); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "考试不存在")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "no_flush", "连接不支持流式推送")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // in case anyone ever puts a proxy in front
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	cursor, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	ctx := r.Context()
	ticker := time.NewTicker(streamEventTick)
	defer ticker.Stop()

	var nextSessions, nextPing time.Time
	for {
		now := time.Now()

		if rows, err := s.st.QueryEvents(ctx, store.EventQuery{
			ExamID: id, AfterID: cursor, Limit: 200, Ascending: true,
		}); err == nil && len(rows) > 0 {
			cursor = rows[len(rows)-1].ID
			if !sseSend(w, flusher, "events", rows) {
				return
			}
		}

		if now.After(nextSessions) {
			nextSessions = now.Add(streamSessionTick)
			if views, err := s.buildSessionViews(ctx, id); err == nil {
				if !sseSend(w, flusher, "sessions", views) {
					return
				}
			}
		}

		if now.After(nextPing) {
			nextPing = now.Add(streamKeepalive)
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}

		select {
		case <-ctx.Done():
			return
		case <-s.stopping:
			return
		case <-ticker.C:
		}
	}
}

// sseSend writes one event frame, reporting whether the client is still there.
func sseSend(w http.ResponseWriter, flusher http.Flusher, name string, payload any) bool {
	body, err := jsonLine(payload)
	if err != nil {
		return true // a payload that will not marshal is a bug, not a disconnect
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, body); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
