package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"coderoom/internal/protocol"
	"coderoom/internal/store"
)

// seedExam creates a running exam, which is the only state the invigilation
// features apply to.
func (h *harness) seedExam() (problemID, examID int64) {
	h.t.Helper()
	var p store.Problem
	status, raw := h.do(http.MethodPost, "/api/teacher/problems", map[string]any{
		"title": "A+B", "time_limit_ms": 2000, "memory_limit_mb": 256,
		"languages": []string{"cpp"},
	}, true)
	h.mustJSON(status, raw, &p)

	var e store.Exam
	status, raw = h.do(http.MethodPost, "/api/teacher/exams", map[string]any{
		"name": "期末上机", "type": "exam", "state": "running",
		"items": []map[string]any{{"problem_id": p.ID, "score": 100}},
	}, true)
	h.mustJSON(status, raw, &e)
	return p.ID, e.ID
}

// A new exam arrives with monitoring on and a new practice session does not.
// During practice a student looking something up is studying, and alerting on
// it would train the teacher to ignore the dashboard.
func TestMonitoringDefaultsDifferByExamType(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	_, practiceID := h.seedPractice()

	get := func(id int64) store.Exam {
		var e store.Exam
		status, raw := h.do(http.MethodGet, "/api/teacher/exams/"+itoa(id), nil, true)
		h.mustJSON(status, raw, &e)
		return e
	}

	exam := get(examID)
	if !exam.Policy.MonitorUSB || !exam.Policy.MonitorProcess ||
		!exam.Policy.MonitorForeground || !exam.Policy.MonitorPaste {
		t.Errorf("an exam should start monitored: %+v", exam.Policy)
	}
	if exam.Policy.MonitorNetwork {
		t.Error("network monitoring is too noisy to be a default")
	}
	if len(exam.Policy.ProcessBlacklist) == 0 {
		t.Error("an exam with process monitoring on needs a blacklist to match against")
	}

	prac := get(practiceID)
	if prac.Policy.MonitorUSB || prac.Policy.MonitorProcess || prac.Policy.MonitorForeground {
		t.Errorf("practice should not be invigilated: %+v", prac.Policy)
	}
	if !prac.Policy.MonitorPaste {
		t.Error("paste tracking is cheap and useful everywhere; it should stay on")
	}
}

// Saving a policy must bump the version, because that is the entire mechanism
// by which fifty machines pick the change up - including the one that was
// still booting when the teacher pressed save.
func TestPolicySaveBumpsVersionAndClientsPullIt(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	login := h.login(examID, "2023", "张三", "")
	if login.Policy.MonitorNetwork {
		t.Fatal("precondition: network monitoring should start off")
	}

	var saved protocol.Policy
	status, raw := h.do(http.MethodPut, "/api/teacher/exams/"+itoa(examID)+"/policy",
		map[string]any{
			"monitor_usb": true, "monitor_network": true, "monitor_paste": true,
			"process_blacklist": []string{"chrome.exe", "CHROME", " ", "msedge"},
			"process_whitelist": []string{"devcpp.exe"},
		}, true)
	h.mustJSON(status, raw, &saved)

	if saved.Version <= login.PolicyVersion {
		t.Fatalf("version did not advance: was %d, now %d", login.PolicyVersion, saved.Version)
	}
	// Duplicates differing only in case or whitespace are one entry: the list
	// is edited as free text and a teacher should not have to be careful.
	if len(saved.ProcessBlacklist) != 2 {
		t.Errorf("blacklist not de-duplicated: %v", saved.ProcessBlacklist)
	}

	// The client is told on its next heartbeat, without anything being pushed.
	var hb protocol.HeartbeatResponse
	status, raw = h.do(http.MethodPost, "/api/client/heartbeat",
		protocol.HeartbeatRequest{PolicyVersion: login.PolicyVersion}, false)
	h.mustJSON(status, raw, &hb)
	if hb.Policy == nil {
		t.Fatal("a client on the old version was not handed the new policy")
	}
	if !hb.Policy.MonitorNetwork || hb.Policy.Version != saved.Version {
		t.Errorf("wrong policy delivered: %+v", hb.Policy)
	}
}

// Regression: a saved policy must survive the round trip even when it turns
// everything off. Deciding "was a policy ever saved?" from a field inside the
// policy made an all-off save indistinguishable from no save at all, and the
// defaults came back instead - so the teacher's change vanished with no error
// anywhere.
func TestAnAllOffPolicySurvivesTheRoundTrip(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()

	status, raw := h.do(http.MethodPut, "/api/teacher/exams/"+itoa(examID)+"/policy",
		map[string]any{
			"monitor_usb": false, "monitor_process": false, "monitor_foreground": false,
			"monitor_paste": false, "monitor_network": false,
			"process_blacklist": []string{}, "process_whitelist": []string{},
		}, true)
	h.mustJSON(status, raw, nil)

	var e store.Exam
	status, raw = h.do(http.MethodGet, "/api/teacher/exams/"+itoa(examID), nil, true)
	h.mustJSON(status, raw, &e)

	if len(monitorSummary(e.Policy)) != 0 {
		t.Fatalf("a deliberately empty policy came back populated: %+v", e.Policy)
	}
	if len(e.Policy.ProcessBlacklist) != 0 {
		t.Errorf("blacklist resurrected: %v", e.Policy.ProcessBlacklist)
	}
	if e.Policy.Version != e.PolicyVersion {
		t.Errorf("policy version %d disagrees with the column %d",
			e.Policy.Version, e.PolicyVersion)
	}
}

// The enforcement flags exist on the wire so M4 needs no protocol change, but
// nothing implements them. Accepting one would leave a teacher believing USB
// storage is disabled when it is not - worse than having no such switch.
func TestEnforcementFlagsCannotBeTurnedOnYet(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()

	var saved protocol.Policy
	status, raw := h.do(http.MethodPut, "/api/teacher/exams/"+itoa(examID)+"/policy",
		map[string]any{
			"block_usb_storage": true, "block_internet": true,
			"allowed_hosts": []string{"192.168.1.10"},
		}, true)
	h.mustJSON(status, raw, &saved)

	if saved.BlockUSBStorage || saved.BlockInternet || len(saved.AllowedHosts) > 0 {
		t.Errorf("enforcement accepted before it exists: %+v", saved)
	}
}

func TestPolicyIntervalsAreClamped(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()

	var saved protocol.Policy
	status, raw := h.do(http.MethodPut, "/api/teacher/exams/"+itoa(examID)+"/policy",
		map[string]any{"snapshot_interval_sec": 1, "heartbeat_interval_sec": 9999}, true)
	h.mustJSON(status, raw, &saved)

	if saved.SnapshotIntervalSec != 10 {
		t.Errorf("snapshot interval not clamped up: %d", saved.SnapshotIntervalSec)
	}
	if saved.HeartbeatIntervalSec != 120 {
		t.Errorf("heartbeat interval not clamped down: %d", saved.HeartbeatIntervalSec)
	}
}

// The heartbeat carries what the student is looking at, so the dashboard can
// answer "where is he now" without replaying the event log.
func TestHeartbeatRecordsForegroundForTheDashboard(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	h.login(examID, "2023", "张三", "")

	status, raw := h.do(http.MethodPost, "/api/client/heartbeat",
		protocol.HeartbeatRequest{Focused: false, Foreground: "chrome.exe"}, false)
	h.mustJSON(status, raw, nil)

	var views []struct {
		StudentNo  string `json:"student_no"`
		Focused    bool   `json:"focused"`
		Foreground string `json:"foreground"`
		Online     bool   `json:"online"`
	}
	status, raw = h.do(http.MethodGet, "/api/teacher/exams/"+itoa(examID)+"/sessions", nil, true)
	h.mustJSON(status, raw, &views)
	if len(views) != 1 {
		t.Fatalf("got %d sessions, want 1", len(views))
	}
	if views[0].Foreground != "chrome.exe" || views[0].Focused {
		t.Errorf("client state not recorded: %+v", views[0])
	}
	if !views[0].Online {
		t.Error("a session that just beat must read as online")
	}
}

// A machine whose clock is simply set wrong must not produce an event on
// every heartbeat. At six beats a minute that is seven hundred identical rows
// over a two-hour exam, which would push every real alert off the dashboard.
func TestClockSkewIsReportedOnChangeNotOnEveryHeartbeat(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	h.login(examID, "2023", "张三", "")

	beat := func(offsetMS int64) {
		h.t.Helper()
		status, raw := h.do(http.MethodPost, "/api/client/heartbeat",
			protocol.HeartbeatRequest{ClientTime: store.NowMS() - offsetMS}, false)
		h.mustJSON(status, raw, nil)
	}
	skewEvents := func() []store.EventRow {
		h.t.Helper()
		var rows []store.EventRow
		status, raw := h.do(http.MethodGet,
			"/api/teacher/exams/"+itoa(examID)+"/events?limit=500", nil, true)
		h.mustJSON(status, raw, &rows)
		var out []store.EventRow
		for _, r := range rows {
			if r.Type == protocol.EvClockSkew {
				out = append(out, r)
			}
		}
		return out
	}

	for i := 0; i < 5; i++ {
		beat(600_000) // ten minutes slow, steadily
	}
	if n := len(skewEvents()); n != 1 {
		t.Fatalf("a steady wrong clock produced %d events, want 1", n)
	}

	// The clock is moved again mid-exam: that is a new fact, and is reported.
	for i := 0; i < 3; i++ {
		beat(3_600_000)
	}
	if n := len(skewEvents()); n != 2 {
		t.Fatalf("a clock change mid-exam produced %d events in total, want 2", n)
	}

	// Back in step: nothing further.
	beat(0)
	beat(0)
	if n := len(skewEvents()); n != 2 {
		t.Fatalf("a corrected clock added events: %d", n)
	}
}

// --------------------------------------------------------- heartbeat lost

// The one alert a client cannot raise for itself. A student who kills the
// process goes quiet, and quiet is also what a well behaved student looks
// like; watching for absence from this side is what separates the two.
func TestLostHeartbeatIsClaimedExactlyOnce(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	login := h.login(examID, "2023", "张三", "")
	ctx := context.Background()

	// Not yet overdue.
	got, err := h.st.ClaimSilentSessions(ctx, store.NowMS()-heartbeatGraceMS)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("claimed a live session: %+v", got)
	}

	// A killed client: no heartbeat for longer than the grace period. The
	// cutoff is moved instead of the clock, which is the same arithmetic the
	// sweeper does and needs no waiting.
	got, err = h.st.ClaimSilentSessions(ctx, overdue())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d offline sessions, want 1", len(got))
	}

	// A second sweep must not re-report the same episode: the latch is what
	// keeps one dead client from producing an alert every ten seconds.
	got, err = h.st.ClaimSilentSessions(ctx, overdue())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("same episode claimed twice: %+v", got)
	}

	// The client comes back, then dies again: a new episode, reported again.
	status, raw := h.do(http.MethodPost, "/api/client/heartbeat",
		protocol.HeartbeatRequest{}, false)
	h.mustJSON(status, raw, nil)
	got, err = h.st.ClaimSilentSessions(ctx, overdue())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("a second episode was not reported: %d claimed", len(got))
	}
	_ = login
}

func TestLostHeartbeatIgnoresPracticeAndHandedInStudents(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Practice: closing the client is just leaving.
	_, practiceID := h.seedPractice()
	h.login(practiceID, "2001", "练习生", "")

	// An exam student who has handed in: same thing.
	_, examID := h.seedExam()
	h.login(examID, "2002", "已交卷", "")
	status, raw := h.do(http.MethodPost, "/api/client/handin", nil, false)
	h.mustJSON(status, raw, nil)

	got, err := h.st.ClaimSilentSessions(ctx, overdue())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		for _, s := range got {
			t.Errorf("should not have alerted for %s", s.StudentNo)
		}
	}
}

// overdue is a cutoff far enough in the future that every existing heartbeat
// counts as stale, which is how these tests age a session without sleeping.
func overdue() int64 { return store.NowMS() + 60_000 }

// --------------------------------------------------------------- dashboard

func TestEventTallyPerStudent(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	h.login(examID, "2023", "张三", "")

	status, raw := h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{
			{Type: protocol.EvUSBInserted, Severity: protocol.SevAlert},
			{Type: protocol.EvPasteLarge, Severity: protocol.SevWarn},
			{Type: protocol.EvPaste, Severity: protocol.SevInfo},
		},
	}, false)
	h.mustJSON(status, raw, nil)

	var views []sessionView
	status, raw = h.do(http.MethodGet, "/api/teacher/exams/"+itoa(examID)+"/sessions", nil, true)
	h.mustJSON(status, raw, &views)
	if len(views) != 1 {
		t.Fatalf("got %d sessions", len(views))
	}
	// client_started is logged at login and is info, so it must not inflate
	// the counts a teacher scans the room by.
	if views[0].Events.Alert != 1 || views[0].Events.Warn != 1 {
		t.Errorf("tally wrong: %+v", views[0].Events)
	}
}

func TestEventsCanBeNarrowedToOneStudent(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()

	h.login(examID, "2001", "甲", "")
	status, raw := h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{{Type: protocol.EvUSBInserted, Severity: protocol.SevAlert}},
	}, false)
	h.mustJSON(status, raw, nil)

	h.login(examID, "2002", "乙", "")
	status, raw = h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{{Type: protocol.EvExternalNet, Severity: protocol.SevWarn}},
	}, false)
	h.mustJSON(status, raw, nil)

	var rows []store.EventRow
	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(examID)+"/events?student=2002", nil, true)
	h.mustJSON(status, raw, &rows)
	if len(rows) == 0 {
		t.Fatal("no events for the student asked about")
	}
	for _, r := range rows {
		if r.StudentNo != "2002" {
			t.Fatalf("timeline leaked another student: %+v", r)
		}
	}

	status, raw = h.do(http.MethodGet,
		"/api/teacher/exams/"+itoa(examID)+"/events?severity=alert", nil, true)
	h.mustJSON(status, raw, &rows)
	for _, r := range rows {
		if r.Severity != protocol.SevAlert {
			t.Fatalf("severity filter ignored: %+v", r)
		}
	}
}

// The stream's cursor is the single source of truth for what the dashboard has
// seen, so a reconnect resumes exactly where it stopped - nothing buffered
// server-side, nothing missed.
func TestStreamResumesFromCursor(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	h.login(examID, "2023", "张三", "")

	status, raw := h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{{Type: protocol.EvUSBInserted, Severity: protocol.SevAlert}},
	}, false)
	h.mustJSON(status, raw, nil)

	var before []store.EventRow
	status, raw = h.do(http.MethodGet, "/api/teacher/exams/"+itoa(examID)+"/events", nil, true)
	h.mustJSON(status, raw, &before)
	if len(before) == 0 {
		t.Fatal("no events recorded")
	}
	cursor := before[0].ID // newest first

	// Something happens while the teacher's tab is disconnected.
	status, raw = h.do(http.MethodPost, "/api/client/events", protocol.EventsRequest{
		Events: []protocol.Event{{Type: protocol.EvProcBlacklisted, Severity: protocol.SevAlert,
			Payload: map[string]any{"process": "chrome.exe"}}},
	}, false)
	h.mustJSON(status, raw, nil)

	frames := readStream(t, h, examID, cursor, "events")
	var rows []store.EventRow
	if err := json.Unmarshal([]byte(frames), &rows); err != nil {
		t.Fatalf("stream frame is not an event list: %s", frames)
	}
	if len(rows) != 1 || rows[0].Type != protocol.EvProcBlacklisted {
		t.Fatalf("stream did not resume from the cursor: %+v", rows)
	}
	if rows[0].Payload["process"] != "chrome.exe" {
		t.Errorf("payload lost in the stream: %+v", rows[0].Payload)
	}
}

func TestStreamSendsSessionState(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()
	h.login(examID, "2023", "张三", "")

	frames := readStream(t, h, examID, 0, "sessions")
	var views []sessionView
	if err := json.Unmarshal([]byte(frames), &views); err != nil {
		t.Fatalf("sessions frame malformed: %s", frames)
	}
	if len(views) != 1 || views[0].StudentNo != "2023" {
		t.Fatalf("unexpected sessions frame: %+v", views)
	}
}

func TestStreamRequiresAdminToken(t *testing.T) {
	h := newHarness(t)
	_, examID := h.seedExam()

	// EventSource cannot set a header, so the stream has to accept the token
	// in the query string - and must still reject a wrong one.
	res, err := http.Get(h.ts.URL + "/api/teacher/exams/" + itoa(examID) + "/stream?t=nope")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stream accepted a bad token: HTTP %d", res.StatusCode)
	}
}

// readStream opens the SSE endpoint and returns the data of the first frame
// with the given event name.
func readStream(t *testing.T, h *harness, examID, cursor int64, want string) string {
	t.Helper()
	url := h.ts.URL + "/api/teacher/exams/" + itoa(examID) +
		"/stream?since=" + itoa(cursor) + "&t=test-admin-token"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("stream: HTTP %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("wrong content type %q", ct)
	}

	sc := bufio.NewScanner(res.Body)
	var name string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if name == want {
				return strings.TrimPrefix(line, "data: ")
			}
		}
	}
	t.Fatalf("stream closed without a %q frame", want)
	return ""
}
