package client

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"coderoom/internal/protocol"
)

//go:embed all:web
var webFS embed.FS

// App is the client core. The UI runs in a WebView2 window pointed at a local
// HTTP server; this type is both that server and the state behind it. Keeping
// the UI on the other side of an HTTP boundary is what makes the interface
// layer replaceable (a Win7 fallback would swap the window, not this code).
type App struct {
	mu      sync.RWMutex
	dataDir string
	logger  *log.Logger
	local   *LocalStore
	up      *Upstream

	discoveryPort int

	// state guarded by mu
	connected      bool
	loggedIn       bool
	exams          []protocol.ExamBrief
	exam           protocol.ExamBrief
	policy         protocol.Policy
	paper          *protocol.PaperResponse
	class          string
	results        []protocol.SubmissionResult
	resultsVisible bool
	resultsReason  string
	serverOffsetMS int64
	lastSyncAt     int64
	lastError      string
	handedIn       bool
	focused        bool
	foreground     string // live foreground app, from the OS monitor

	mon *monitor
	mux *http.ServeMux
}

func NewApp(dataDir string, discoveryPort int, logger *log.Logger) (*App, error) {
	if logger == nil {
		logger = log.Default()
	}
	local, err := OpenLocalStore(dataDir)
	if err != nil {
		return nil, err
	}
	a := &App{
		dataDir:       dataDir,
		logger:        logger,
		local:         local,
		discoveryPort: discoveryPort,
		policy:        protocol.DefaultPolicy(),
		focused:       true,
	}
	if addr := local.State().ServerAddr; addr != "" {
		a.up = NewUpstream(addr)
	}
	a.mon = newMonitor(monitorDeps{
		probe: newSysProbe(),
		// Monitoring is scoped to a logged-in session. Before login there is
		// no student to attribute an event to, and no policy that a teacher
		// chose - so the monitor is handed an empty policy and does nothing.
		policy: func() protocol.Policy {
			a.mu.RLock()
			defer a.mu.RUnlock()
			if !a.loggedIn {
				return protocol.Policy{}
			}
			return a.policy
		},
		emit:          local.QueueEvent,
		setForeground: a.noteForeground,
		serverHost: func() string {
			a.mu.RLock()
			defer a.mu.RUnlock()
			if a.up == nil {
				return ""
			}
			return a.up.Addr()
		},
		logf: logger.Printf,
	})
	a.routes()
	return a, nil
}

func (a *App) noteForeground(name string) {
	a.mu.Lock()
	a.foreground = name
	a.mu.Unlock()
}

// osForegroundActive reports whether the OS-level foreground monitor is doing
// the watching. When it is, the browser's own blur event is redundant and
// strictly less informative - it knows focus was lost but not to what - so the
// UI stops reporting and the timeline holds one event per switch, not two.
func (a *App) osForegroundActive() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.policy.MonitorForeground && a.mon != nil && a.mon.canWatchForeground()
}

// ServerTime is the only clock the UI is allowed to use for the countdown.
// The local clock is treated as untrusted and only contributes the delta since
// the last server response.
func (a *App) ServerTime() int64 {
	a.mu.RLock()
	off := a.serverOffsetMS
	a.mu.RUnlock()
	return time.Now().UnixMilli() + off
}

func (a *App) noteServerTime(serverTime int64) {
	if serverTime == 0 {
		return
	}
	a.mu.Lock()
	a.serverOffsetMS = serverTime - time.Now().UnixMilli()
	a.lastSyncAt = time.Now().UnixMilli()
	a.connected = true
	a.lastError = ""
	a.mu.Unlock()
}

func (a *App) noteError(err error) {
	if err == nil {
		return
	}
	a.mu.Lock()
	a.lastError = err.Error()
	a.connected = false
	a.mu.Unlock()
}

// ------------------------------------------------------------- local routes

func (a *App) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /local/state", a.hState)
	mux.HandleFunc("POST /local/discover", a.hDiscover)
	mux.HandleFunc("POST /local/connect", a.hConnect)
	mux.HandleFunc("POST /local/login", a.hLogin)
	mux.HandleFunc("POST /local/logout", a.hLogout)
	mux.HandleFunc("POST /local/draft", a.hDraft)
	mux.HandleFunc("POST /local/submit", a.hSubmit)
	mux.HandleFunc("POST /local/sync", a.hSync)
	mux.HandleFunc("POST /local/handin", a.hHandIn)
	mux.HandleFunc("POST /local/event", a.hEvent)
	mux.HandleFunc("POST /local/focus", a.hFocus)
	mux.HandleFunc("POST /local/export", a.hExport)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	a.mux = mux
}

// Listen binds a loopback-only listener. Loopback-only matters: the student UI
// API can save and submit code, and must not be reachable from the network.
func (a *App) Listen() (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, "http://" + ln.Addr().String(), nil
}

func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: a.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --------------------------------------------------------------- UI payload

type uiState struct {
	Version        string                      `json:"version"`
	ServerAddr     string                      `json:"server_addr"`
	Connected      bool                        `json:"connected"`
	LoggedIn       bool                        `json:"logged_in"`
	Exams          []protocol.ExamBrief        `json:"exams"`
	Exam           *protocol.ExamBrief         `json:"exam"`
	StudentNo      string                      `json:"student_no"`
	Name           string                      `json:"name"`
	Class          string                      `json:"class"`
	Paper          *protocol.PaperResponse     `json:"paper"`
	Drafts         map[string]*Draft           `json:"drafts"`
	Results        []protocol.SubmissionResult `json:"results"`
	ResultsVisible bool                        `json:"results_visible"`
	ResultsReason  string                      `json:"results_reason"`
	ServerTime     int64                       `json:"server_time"`
	Deadline       *int64                      `json:"deadline"`
	PendingEvents  int                         `json:"pending_events"`
	PendingUploads int                         `json:"pending_uploads"`
	LastSyncAt     int64                       `json:"last_sync_at"`
	LastError      string                      `json:"last_error"`
	HandedIn       bool                        `json:"handed_in"`
	Policy         protocol.Policy             `json:"policy"`
	// Monitors is the plain-language list of checks in force, shown to the
	// student. A monitored exam says so: visible rules produce fewer disputes
	// afterwards, and a hidden check deters nobody.
	Monitors []string `json:"monitors"`
}

func (a *App) snapshotUI() uiState {
	a.mu.RLock()
	defer a.mu.RUnlock()

	ls := a.local.State()
	drafts := make(map[string]*Draft, len(ls.Drafts))
	pendingUploads := 0
	for id, d := range ls.Drafts {
		drafts[itoa(id)] = d
		if d.Dirty() {
			pendingUploads++
		}
	}

	st := uiState{
		Version:        Version,
		Connected:      a.connected,
		LoggedIn:       a.loggedIn,
		Exams:          a.exams,
		StudentNo:      ls.StudentNo,
		Name:           ls.Name,
		Class:          a.class,
		Paper:          a.paper,
		Drafts:         drafts,
		Results:        a.results,
		ResultsVisible: a.resultsVisible,
		ResultsReason:  a.resultsReason,
		ServerTime:     time.Now().UnixMilli() + a.serverOffsetMS,
		PendingEvents:  len(ls.PendingEvents),
		PendingUploads: pendingUploads,
		LastSyncAt:     a.lastSyncAt,
		LastError:      a.lastError,
		HandedIn:       a.handedIn,
		Policy:         a.policy,
		Monitors:       []string{},
	}
	if a.loggedIn {
		st.Monitors = activeMonitors(a.policy)
	}
	if a.up != nil {
		st.ServerAddr = a.up.Addr()
	}
	if a.exams == nil {
		st.Exams = []protocol.ExamBrief{}
	}
	if a.results == nil {
		st.Results = []protocol.SubmissionResult{}
	}
	if a.loggedIn {
		e := a.exam
		st.Exam = &e
	}
	if a.paper != nil {
		st.Deadline = a.paper.Deadline
	}
	return st
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func (a *App) hState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

func (a *App) hDiscover(w http.ResponseWriter, r *http.Request) {
	found, err := DiscoverServers(r.Context(), a.discoveryPort, 1200*time.Millisecond)
	if err != nil {
		// Broadcast being unavailable is normal on locked-down networks; the
		// UI falls back to a manually typed address.
		a.logger.Printf("discover: %v", err)
	}
	if found == nil {
		found = []Discovered{}
	}
	writeJSON(w, http.StatusOK, found)
}

// Connect points the client at a server and fetches the open exam list. Used
// both by the UI and by the -server startup flag, so a lab with broadcast
// disabled gets a client that is already connected before the UI even loads.
func (a *App) Connect(ctx context.Context, rawAddr string) error {
	addr, err := NormalizeAddr(rawAddr)
	if err != nil {
		return err
	}
	up := NewUpstream(addr)
	list, err := up.ListExams(ctx)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.up = up
	a.exams = list.Exams
	a.connected = true
	a.lastError = ""
	a.mu.Unlock()
	a.noteServerTime(list.ServerTime)
	// Remembering the address means the next launch skips discovery too.
	_ = a.local.Update(func(st *LocalState) { st.ServerAddr = addr })
	return nil
}

func (a *App) hConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr string `json:"addr"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := a.Connect(r.Context(), body.Addr); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

func machineInfo() protocol.MachineInfo {
	host, _ := os.Hostname()
	return protocol.MachineInfo{
		Hostname:      host,
		MAC:           primaryMAC(),
		OS:            runtime.GOOS,
		ClientVersion: Version,
	}
}

func primaryMAC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagLoopback != 0 || len(i.HardwareAddr) == 0 {
			continue
		}
		if i.Flags&net.FlagUp != 0 {
			return i.HardwareAddr.String()
		}
	}
	return ""
}

func (a *App) hLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExamID    int64  `json:"exam_id"`
		StudentNo string `json:"student_no"`
		Name      string `json:"name"`
		Passcode  string `json:"passcode"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		writeErr(w, http.StatusBadRequest, "请先连接服务端")
		return
	}

	res, err := up.Login(r.Context(), protocol.LoginRequest{
		ExamID:     body.ExamID,
		StudentNo:  body.StudentNo,
		Name:       body.Name,
		Passcode:   body.Passcode,
		Machine:    machineInfo(),
		ClientTime: time.Now().UnixMilli(),
	})
	if err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	a.noteServerTime(res.ServerTime)

	// A different student (or exam) on this machine must not inherit the
	// previous occupant's drafts.
	prev := a.local.State()
	fresh := prev.StudentNo != res.StudentNo || prev.ExamID != res.Exam.ID
	if fresh && len(prev.PendingEvents) > 0 {
		// Events are attributed by session token, so anything still queued
		// from the previous occupant would be filed under this student. Being
		// flagged for someone else's USB stick is far worse than the loss, and
		// a student logging back in after a crash keeps their own queue
		// because the number and exam still match.
		a.logger.Printf("本机换了学生（%s -> %s），丢弃上一位留下的 %d 条未上报事件",
			prev.StudentNo, res.StudentNo, len(prev.PendingEvents))
	}
	_ = a.local.Update(func(st *LocalState) {
		st.StudentNo = res.StudentNo
		st.Name = res.Name
		st.ExamID = res.Exam.ID
		if fresh {
			st.Drafts = map[int64]*Draft{}
			st.PendingEvents = nil
		}
	})

	a.mu.Lock()
	a.loggedIn = true
	a.exam = res.Exam
	a.policy = res.Policy
	a.class = res.Class
	a.handedIn = false
	a.results = nil
	a.mu.Unlock()

	a.refreshPaper(r.Context())
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

func (a *App) hLogout(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.loggedIn = false
	a.paper = nil
	a.results = nil
	if a.up != nil {
		a.up.SetToken("")
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

// refreshPaper pulls the paper. Statements arrive only once the exam is open;
// before that the response is deliberately empty of content.
func (a *App) refreshPaper(ctx context.Context) {
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		return
	}
	paper, err := up.Paper(ctx)
	if err != nil {
		a.noteError(err)
		return
	}
	a.noteServerTime(paper.ServerTime)
	a.local.AdoptServerDrafts(paper.Drafts)

	a.mu.Lock()
	a.paper = paper
	a.mu.Unlock()
}

func (a *App) hDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProblemID int64  `json:"problem_id"`
		Language  string `json:"language"`
		Code      string `json:"code"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	// Saved locally first, unconditionally. Upload happens on the snapshot
	// tick; if the network is down the work is still safe on disk.
	if err := a.local.SaveDraft(body.ProblemID, body.Language, body.Code); err != nil {
		writeErr(w, http.StatusInternalServerError, "本地保存失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved_at": time.Now().UnixMilli()})
}

func (a *App) hSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProblemID int64  `json:"problem_id"`
		Language  string `json:"language"`
		Code      string `json:"code"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		writeErr(w, http.StatusBadRequest, "未连接服务端")
		return
	}
	// Save locally before attempting the upload, so a failed submit still
	// leaves the student's latest code recorded.
	_ = a.local.SaveDraft(body.ProblemID, body.Language, body.Code)

	res, err := up.Submit(r.Context(), protocol.SubmitRequest{
		ProblemID:  body.ProblemID,
		Language:   body.Language,
		Code:       body.Code,
		Hash:       HashCode(body.Code),
		ClientTime: time.Now().UnixMilli(),
	})
	if err != nil {
		a.noteError(err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.noteServerTime(res.ServerTime)
	a.local.MarkUploaded(body.ProblemID, HashCode(body.Code))
	a.refreshResults(r.Context())

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "submission_id": res.SubmissionID, "message": res.Message,
	})
}

func (a *App) hSync(w http.ResponseWriter, r *http.Request) {
	a.uploadDirtyDrafts(r.Context())
	a.flushEvents(r.Context())
	a.refreshPaper(r.Context())
	a.refreshResults(r.Context())
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

func (a *App) hHandIn(w http.ResponseWriter, r *http.Request) {
	a.uploadDirtyDrafts(r.Context())
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		writeErr(w, http.StatusBadRequest, "未连接服务端")
		return
	}
	if err := up.HandIn(r.Context()); err != nil {
		a.noteError(err)
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	a.mu.Lock()
	a.handedIn = true
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.snapshotUI())
}

func (a *App) hEvent(w http.ResponseWriter, r *http.Request) {
	var ev protocol.Event
	if err := readJSON(r, &ev); err != nil || ev.Type == "" {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if ev.ClientTime == 0 {
		ev.ClientTime = time.Now().UnixMilli()
	}
	a.local.QueueEvent(ev)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) hFocus(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Focused bool `json:"focused"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	a.mu.Lock()
	was := a.focused
	a.focused = body.Focused
	monitor := a.policy.MonitorForeground
	a.mu.Unlock()

	// Losing focus means another window came forward. Reported only when the
	// policy asks for it, only on the transition, and only when the OS-level
	// monitor is not already covering this with better detail.
	if monitor && was && !body.Focused && !a.osForegroundActive() {
		a.local.QueueEvent(protocol.Event{
			Type:       protocol.EvForegroundLost,
			Severity:   protocol.SevWarn,
			ClientTime: time.Now().UnixMilli(),
			Payload:    map[string]any{"source": "ui"},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) hExport(w http.ResponseWriter, r *http.Request) {
	titles := map[int64]string{}
	a.mu.RLock()
	if a.paper != nil {
		for _, it := range a.paper.Items {
			if it.Problem != nil {
				titles[it.ProblemID] = it.Problem.Title
			}
		}
	}
	a.mu.RUnlock()

	path, err := a.local.ExportAnswers(a.dataDir, titles)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "导出失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}

// --------------------------------------------------------------- background

// Run drives the periodic work: heartbeat (which also carries policy sync),
// snapshot upload, event flush and result refresh. One loop with per-task due
// times keeps the policy's intervals adjustable at runtime without restarting
// goroutines.
func (a *App) Run(ctx context.Context) {
	// The behaviour monitor gets its own goroutine: its checks poll the OS and
	// must not be able to delay a heartbeat or a snapshot upload by blocking.
	go a.mon.Run(ctx)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	var nextHeartbeat, nextSnapshot, nextResults time.Time
	for {
		select {
		case <-ctx.Done():
			a.shutdownFlush()
			return
		case now := <-ticker.C:
			a.mu.RLock()
			loggedIn, policy := a.loggedIn, a.policy
			a.mu.RUnlock()
			if !loggedIn {
				continue
			}

			hbEvery := dur(policy.HeartbeatIntervalSec, 10)
			snapEvery := dur(policy.SnapshotIntervalSec, 30)

			if now.After(nextHeartbeat) {
				nextHeartbeat = now.Add(hbEvery)
				a.heartbeat(ctx)
				a.flushEvents(ctx)
			}
			if now.After(nextSnapshot) {
				nextSnapshot = now.Add(snapEvery)
				a.uploadDirtyDrafts(ctx)
			}
			if now.After(nextResults) {
				nextResults = now.Add(5 * time.Second)
				a.refreshResults(ctx)
			}
		}
	}
}

func dur(sec, fallback int) time.Duration {
	if sec <= 0 {
		sec = fallback
	}
	if sec < 2 {
		sec = 2
	}
	return time.Duration(sec) * time.Second
}

// shutdownFlushTimeout bounds the last upload attempt. On a working LAN the
// whole flush takes milliseconds; if it has not gone through in three seconds
// the server is unreachable and waiting longer only keeps a window-less
// process alive in front of the student. Nothing is lost by giving up: the
// queue is on disk and goes out on the next launch.
const shutdownFlushTimeout = 3 * time.Second

// shutdownFlush makes a last attempt to push unsent work when the student
// closes the client normally.
//
// The exit event is what separates "closed the client" from "killed the
// process" on the teacher's dashboard - the latter shows up only as a lost
// heartbeat - so it is worth the wait in Run's caller.
func (a *App) shutdownFlush() {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
	defer cancel()

	// Only a logged-in client has a session to attribute the event to. Queued
	// without one it would sit on disk and be delivered under whichever
	// student logs in next.
	if a.isLoggedIn() {
		a.local.QueueEvent(protocol.Event{
			Type:       protocol.EvClientExiting,
			Severity:   protocol.SevInfo,
			ClientTime: time.Now().UnixMilli(),
		})
	}
	a.uploadDirtyDrafts(ctx)
	a.flushEvents(ctx)

	// Say what actually left the machine. This is the line a teacher needs
	// when the dashboard shows no exit for a student, and the line a student
	// needs when asked to hand in their log after a disputed exam.
	ls := a.local.State()
	stuck := 0
	for _, d := range ls.Drafts {
		if d.Dirty() {
			stuck++
		}
	}
	switch {
	case len(ls.PendingEvents) == 0 && stuck == 0:
		a.logger.Printf("客户端退出：代码与事件均已上报服务端")
	default:
		a.logger.Printf("客户端退出：仍有 %d 份代码、%d 条事件未能上报（服务端不可达），"+
			"已保留在本机 %s，下次登录后自动补发",
			stuck, len(ls.PendingEvents), a.dataDir)
	}
}

func (a *App) isLoggedIn() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.loggedIn
}

func (a *App) heartbeat(ctx context.Context) {
	a.mu.RLock()
	up, pv, fg := a.up, a.policy.Version, a.foreground
	a.mu.RUnlock()
	if up == nil {
		return
	}
	res, err := up.Heartbeat(ctx, protocol.HeartbeatRequest{
		ClientTime:    time.Now().UnixMilli(),
		Focused:       a.isFocused(),
		PolicyVersion: pv,
		Foreground:    fg,
	})
	if err != nil {
		a.noteError(err)
		return
	}
	a.noteServerTime(res.ServerTime)

	a.mu.Lock()
	a.exam.State = res.ExamState
	if a.paper != nil {
		a.paper.Deadline = res.Deadline
	}
	// Policy arrives whenever the client's version is stale. This is the
	// mechanism that makes a late-booting or rebooted machine self-correct,
	// instead of silently missing a push it was not around for.
	if res.Policy != nil {
		a.policy = *res.Policy
		a.logger.Printf("策略已更新至版本 %d", a.policy.Version)
	}
	wasLocked := a.paper == nil || a.paper.Locked
	a.mu.Unlock()

	// The paper unlocks the moment the teacher starts the exam, so a client
	// sitting on a locked paper re-fetches as soon as the state moves.
	if wasLocked && res.ExamState == protocol.ExamStateRunning {
		a.refreshPaper(ctx)
	}
}

func (a *App) isFocused() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.focused
}

// uploadDirtyDrafts pushes every locally-changed draft. This is the "server
// collects code on a timer" requirement, implemented as a client push so it
// works through the student machine's firewall without any inbound port.
func (a *App) uploadDirtyDrafts(ctx context.Context) {
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		return
	}
	ls := a.local.State()
	var items []protocol.SnapshotItem
	for _, d := range ls.Drafts {
		if d.Code == "" || !d.Dirty() {
			continue
		}
		items = append(items, protocol.SnapshotItem{
			ProblemID: d.ProblemID,
			Language:  d.Language,
			Code:      d.Code,
			Hash:      HashCode(d.Code),
		})
	}
	if len(items) == 0 {
		return
	}
	res, err := up.Snapshot(ctx, protocol.SnapshotRequest{
		Items: items, ClientTime: time.Now().UnixMilli(),
	})
	if err != nil {
		a.noteError(err) // keep the drafts dirty; the next tick retries
		return
	}
	a.noteServerTime(res.ServerTime)
	accepted := map[int64]bool{}
	for _, id := range res.Stored {
		accepted[id] = true
	}
	for _, id := range res.Skipped {
		accepted[id] = true
	}
	for _, it := range items {
		if accepted[it.ProblemID] {
			a.local.MarkUploaded(it.ProblemID, it.Hash)
		}
	}
}

func (a *App) flushEvents(ctx context.Context) {
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	if up == nil {
		return
	}
	evs := a.local.TakeEvents(100)
	if len(evs) == 0 {
		return
	}
	if err := up.SendEvents(ctx, protocol.EventsRequest{Events: evs}); err != nil {
		a.local.ReturnEvents(evs) // evidence is never dropped on a network error
		a.noteError(err)
	}
}

func (a *App) refreshResults(ctx context.Context) {
	a.mu.RLock()
	up, loggedIn := a.up, a.loggedIn
	a.mu.RUnlock()
	if up == nil || !loggedIn {
		return
	}
	res, err := up.Results(ctx)
	if err != nil {
		a.noteError(err)
		return
	}
	a.noteServerTime(res.ServerTime)
	a.mu.Lock()
	a.results = res.Results
	a.resultsVisible = res.Visible
	a.resultsReason = res.Reason
	a.mu.Unlock()
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: "error", Detail: detail})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20)).Decode(v)
}
