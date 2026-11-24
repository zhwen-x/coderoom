package client

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"coderoom/internal/protocol"
)

// fakeServer stands in for the judge server: it accepts a login and records
// every event batch the client pushes.
type fakeServer struct {
	*httptest.Server
	mu     sync.Mutex
	events []protocol.Event
	block  chan struct{} // when non-nil, /events waits on it
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/client/exams", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, protocol.ExamListResponse{
			Envelope: protocol.Envelope{ServerTime: time.Now().UnixMilli(), PolicyVersion: 1},
			Exams:    []protocol.ExamBrief{{ID: 1, Name: "测试", Type: protocol.ExamTypeExam}},
		})
	})
	mux.HandleFunc("POST /api/client/login", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.LoginRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusOK, protocol.LoginResponse{
			Envelope:  protocol.Envelope{ServerTime: time.Now().UnixMilli(), PolicyVersion: 1},
			Token:     "tok-" + req.StudentNo,
			StudentNo: req.StudentNo,
			Name:      req.Name,
			Exam:      protocol.ExamBrief{ID: 1, Name: "测试", Type: protocol.ExamTypeExam},
			Policy:    protocol.DefaultPolicyFor(protocol.ExamTypeExam),
		})
	})
	mux.HandleFunc("POST /api/client/events", func(w http.ResponseWriter, r *http.Request) {
		if f.block != nil {
			select {
			case <-f.block:
			case <-r.Context().Done():
				return
			}
		}
		var req protocol.EventsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.events = append(f.events, req.Events...)
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, protocol.Envelope{ServerTime: time.Now().UnixMilli()})
	})
	// Everything else the client may touch on the way through.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, protocol.Envelope{ServerTime: time.Now().UnixMilli()})
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) received() []protocol.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Event(nil), f.events...)
}

func (f *fakeServer) sawType(typ string) bool {
	for _, ev := range f.received() {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func newTestApp(t *testing.T, addr string) *App {
	t.Helper()
	app, err := NewApp(t.TempDir(), 0, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Connect(context.Background(), addr); err != nil {
		t.Fatal(err)
	}
	return app
}

func (a *App) testLogin(t *testing.T, no string) {
	t.Helper()
	a.mu.RLock()
	up := a.up
	a.mu.RUnlock()
	res, err := up.Login(context.Background(), protocol.LoginRequest{
		ExamID: 1, StudentNo: no, Name: no, ClientTime: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = a.local.Update(func(st *LocalState) {
		st.StudentNo = res.StudentNo
		st.ExamID = res.Exam.ID
	})
	a.mu.Lock()
	a.loggedIn = true
	a.exam = res.Exam
	a.policy = res.Policy
	a.mu.Unlock()
}

// Closing the client normally must reach the server. Without it the dashboard
// cannot tell a student who shut the client down from one who killed the
// process - and only the second is worth walking across the room for.
func TestExitEventReachesTheServerBeforeRunReturns(t *testing.T) {
	srv := newFakeServer(t)
	app := newTestApp(t, srv.URL[len("http://"):])
	app.testLogin(t, "2301")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// The guarantee callers rely on: once Run returns, the flush is finished.
	// Polling here instead would hide exactly the race this test exists for.
	if !srv.sawType(protocol.EvClientExiting) {
		t.Fatalf("no exit event reached the server; got %v", typesOf(srv.received()))
	}
	if n := len(app.local.State().PendingEvents); n != 0 {
		t.Errorf("%d events still queued after a successful flush", n)
	}
}

// When the server cannot be reached the exit event must survive on disk rather
// than being dropped, and the flush must still give up promptly: a window-less
// process lingering in front of the student is its own bug.
func TestExitKeepsEvidenceWhenTheServerIsUnreachable(t *testing.T) {
	srv := newFakeServer(t)
	srv.block = make(chan struct{}) // /events never answers
	app := newTestApp(t, srv.URL[len("http://"):])
	app.testLogin(t, "2301")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(shutdownFlushTimeout + 3*time.Second):
		t.Fatal("Run hung instead of giving up on an unreachable server")
	}
	if waited := time.Since(start); waited > shutdownFlushTimeout+2*time.Second {
		t.Errorf("shutdown took %v, want it bounded near %v", waited, shutdownFlushTimeout)
	}

	queued := app.local.State().PendingEvents
	var found bool
	for _, ev := range queued {
		if ev.Type == protocol.EvClientExiting {
			found = true
		}
	}
	if !found {
		t.Errorf("exit event lost instead of being kept for the next launch: %v", typesOf(queued))
	}
	close(srv.block)
}

// A client that was never logged in has no session to attribute an event to,
// so queueing one would park it on disk until some other student logs in.
func TestNoExitEventWithoutASession(t *testing.T) {
	srv := newFakeServer(t)
	app := newTestApp(t, srv.URL[len("http://"):])

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if n := len(app.local.State().PendingEvents); n != 0 {
		t.Errorf("%d events queued with no session to own them", n)
	}
}

// Events are filed by session token, so anything left queued when a different
// student logs in on the same machine would be recorded against them. Being
// flagged for the previous occupant's USB stick is worse than losing the
// record, so the queue is dropped - while a student logging back in after a
// crash keeps their own.
func TestQueuedEventsDoNotFollowTheMachineToTheNextStudent(t *testing.T) {
	srv := newFakeServer(t)
	srv.block = make(chan struct{})
	dir := t.TempDir()
	app, err := NewApp(dir, 0, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Connect(context.Background(), srv.URL[len("http://"):]); err != nil {
		t.Fatal(err)
	}
	app.testLogin(t, "2301")
	app.local.QueueEvent(protocol.Event{Type: protocol.EvUSBInserted, Severity: protocol.SevAlert})
	if len(app.local.State().PendingEvents) != 1 {
		t.Fatal("precondition: one event should be queued")
	}

	// The same student coming back keeps their own queue.
	loginVia(t, app, "2301")
	if n := len(app.local.State().PendingEvents); n != 1 {
		t.Errorf("a student's own queue was dropped on re-login: %d left", n)
	}

	// A different student does not inherit it.
	loginVia(t, app, "2302")
	if n := len(app.local.State().PendingEvents); n != 0 {
		t.Errorf("%d events from the previous student survived into a new session", n)
	}
	close(srv.block)
}

// loginVia goes through the real login handler, which is where the queue is
// reset - calling testLogin would bypass the code under test.
func loginVia(t *testing.T, app *App, no string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/local/login",
		strings.NewReader(`{"exam_id":1,"student_no":"`+no+`","name":"`+no+`"}`))
	w := httptest.NewRecorder()
	app.hLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login %s: HTTP %d %s", no, w.Code, w.Body.String())
	}
}
