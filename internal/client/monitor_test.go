package client

import (
	"testing"
	"time"

	"coderoom/internal/protocol"
)

// fakeProbe stands in for the OS. Every suppression rule in the monitor is a
// judgement call about what counts as noise, and those are the rules most
// likely to be broken by a later change - so they are tested here rather than
// discovered during an exam.
type fakeProbe struct {
	c     probeCaps
	drv   []driveInfo
	procs []procInfo
	fg    fgInfo
	conns []connInfo
	err   error
}

func (f *fakeProbe) caps() probeCaps { return f.c }
func (f *fakeProbe) drives() ([]driveInfo, error) {
	return f.drv, f.err
}
func (f *fakeProbe) processes() ([]procInfo, error)   { return f.procs, f.err }
func (f *fakeProbe) foreground() (fgInfo, error)      { return f.fg, f.err }
func (f *fakeProbe) connections() ([]connInfo, error) { return f.conns, f.err }

type harness struct {
	t      *testing.T
	m      *monitor
	probe  *fakeProbe
	policy protocol.Policy
	now    time.Time
	events []protocol.Event
	fgSeen string
}

func newHarness(t *testing.T, p protocol.Policy) *harness {
	t.Helper()
	h := &harness{
		t:      t,
		probe:  &fakeProbe{c: probeCaps{true, true, true, true}},
		policy: p,
		now:    time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
	}
	h.m = newMonitor(monitorDeps{
		probe:         h.probe,
		policy:        func() protocol.Policy { return h.policy },
		emit:          func(ev protocol.Event) { h.events = append(h.events, ev) },
		setForeground: func(s string) { h.fgSeen = s },
		serverHost:    func() string { return "192.168.1.10:8080" },
		now:           func() time.Time { return h.now },
	})
	return h
}

// advance moves the clock and runs the ticks that would have fired, so a test
// can skip twenty seconds of dwell without sleeping for it.
func (h *harness) advance(d time.Duration) {
	for end := h.now.Add(d); h.now.Before(end); {
		h.now = h.now.Add(time.Second)
		h.m.tick()
	}
}

func (h *harness) tick() { h.m.tick() }

func (h *harness) drain() []protocol.Event {
	out := h.events
	h.events = nil
	return out
}

func (h *harness) wantTypes(want ...string) {
	h.t.Helper()
	got := h.drain()
	if len(got) != len(want) {
		h.t.Fatalf("got %d events %v, want %d %v", len(got), typesOf(got), len(want), want)
	}
	for i := range want {
		if got[i].Type != want[i] {
			h.t.Fatalf("event %d: got %q, want %q (all: %v)", i, got[i].Type, want[i], typesOf(got))
		}
	}
}

func typesOf(evs []protocol.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func monitorAll() protocol.Policy {
	p := protocol.DefaultPolicyFor(protocol.ExamTypeExam)
	p.MonitorNetwork = true
	return p
}

// ------------------------------------------------------------------- drives

func TestDrivesAlreadyMountedAreNotAnInsertion(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.drv = []driveInfo{
		{Letter: "C:", Kind: "fixed", Ready: true},
		{Letter: "D:", Kind: "cdrom", Ready: true},
	}
	h.tick()
	// The baseline tick must stay silent: a machine's own disks are not
	// evidence, and reporting them would mean fifty alerts at login.
	h.wantTypes()
}

func TestRemovableDriveInsertAndRemove(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.drv = []driveInfo{{Letter: "C:", Kind: "fixed", Ready: true}}
	h.tick()
	h.wantTypes()

	h.probe.drv = append(h.probe.drv, driveInfo{Letter: "F:", Kind: "removable", Ready: true})
	h.advance(drivePollInterval)
	evs := h.drain()
	if len(evs) != 1 || evs[0].Type != protocol.EvUSBInserted {
		t.Fatalf("want one usb_inserted, got %v", typesOf(evs))
	}
	if evs[0].Severity != protocol.SevAlert {
		t.Errorf("a removable drive appearing mid-exam is an alert, got %q", evs[0].Severity)
	}
	if evs[0].Payload["drive"] != "F:" {
		t.Errorf("drive letter lost: %+v", evs[0].Payload)
	}

	// Still plugged in: no repeat.
	h.advance(5 * time.Second)
	h.wantTypes()

	h.probe.drv = h.probe.drv[:1]
	h.advance(drivePollInterval)
	h.wantTypes(protocol.EvUSBRemoved)
}

// A card reader keeps its drive letter whether or not a card is in it, so
// watching letters alone would miss the insertion entirely.
func TestCardInsertedIntoAReaderThatAlreadyHadALetter(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.drv = []driveInfo{
		{Letter: "C:", Kind: "fixed", Ready: true},
		{Letter: "E:", Kind: "removable", Ready: false}, // empty slot
	}
	h.tick()
	h.wantTypes()

	h.probe.drv[1].Ready = true
	h.advance(drivePollInterval)
	h.wantTypes(protocol.EvUSBInserted)

	h.probe.drv[1].Ready = false
	h.advance(drivePollInterval)
	h.wantTypes(protocol.EvUSBRemoved)
}

// ---------------------------------------------------------------- processes

func TestBlacklistedProcessReportedOncePerRun(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.procs = []procInfo{{PID: 10, Name: "devcpp.exe"}}
	h.tick()
	h.wantTypes()

	h.probe.procs = append(h.probe.procs, procInfo{PID: 99, Name: "chrome.exe"})
	h.advance(procPollInterval)
	evs := h.drain()
	if len(evs) != 1 || evs[0].Type != protocol.EvProcBlacklisted {
		t.Fatalf("want process_blacklisted, got %v", typesOf(evs))
	}
	if evs[0].Severity != protocol.SevAlert {
		t.Errorf("want alert severity, got %q", evs[0].Severity)
	}

	// Still running three polls later: one alert, not four.
	h.advance(3 * procPollInterval)
	h.wantTypes()

	// Closed and reopened: a new episode, reported again. An invigilator does
	// want to know the browser came back.
	h.probe.procs = h.probe.procs[:1]
	h.advance(procPollInterval)
	h.wantTypes()
	h.probe.procs = append(h.probe.procs, procInfo{PID: 123, Name: "chrome.exe"})
	h.advance(procPollInterval)
	h.wantTypes(protocol.EvProcBlacklisted)
}

func TestWhitelistBeatsBlacklist(t *testing.T) {
	p := monitorAll()
	p.ProcessBlacklist = []string{"chrome"}
	p.ProcessWhitelist = []string{"chrome.exe"}
	h := newHarness(t, p)
	h.probe.procs = []procInfo{{PID: 1, Name: "chrome.exe"}}
	h.tick()
	h.wantTypes()
}

// Matching is on the whole name. A substring match would make "QQ" fire on
// QQBrowser.exe, and a teacher adding "qq" would not expect that.
func TestBlacklistDoesNotSubstringMatch(t *testing.T) {
	p := monitorAll()
	p.ProcessBlacklist = []string{"QQ"}
	h := newHarness(t, p)
	h.probe.procs = []procInfo{{PID: 1, Name: "QQBrowserHelper.exe"}}
	h.tick()
	h.wantTypes()

	h.probe.procs = []procInfo{{PID: 2, Name: "qq.exe"}} // case-insensitive
	h.advance(procPollInterval)
	h.wantTypes(protocol.EvProcBlacklisted)
}

// --------------------------------------------------------------- foreground

func TestForegroundInsideTheIDEIsNeverReported(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.fg = fgInfo{PID: 7, Process: "devenv.exe", Title: "Project1 - Microsoft Visual Studio"}
	h.advance(5 * fgDwell)
	h.wantTypes()
	if h.fgSeen != "devenv.exe" {
		t.Errorf("the live foreground must be published even when no event fires, got %q", h.fgSeen)
	}
}

func TestForegroundReportedOnlyAfterDwell(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.fg = fgInfo{PID: 7, Process: "devcpp.exe"}
	h.tick()

	// A brief switch - clicking the taskbar, their own program flashing a
	// console - must not produce anything.
	h.probe.fg = fgInfo{PID: 50, Process: "a.exe", Title: "C:\\Users\\s\\a.exe"}
	h.advance(fgDwell - 5*time.Second)
	h.wantTypes()
	h.probe.fg = fgInfo{PID: 7, Process: "devcpp.exe"}
	h.advance(2 * time.Second)
	h.wantTypes()

	// Settling into a non-whitelisted window does.
	h.probe.fg = fgInfo{PID: 60, Process: "notepad.exe", Title: "答案.txt - 记事本"}
	h.advance(fgDwell + time.Second)
	evs := h.drain()
	if len(evs) != 1 || evs[0].Type != protocol.EvForegroundLost {
		t.Fatalf("want one foreground_lost, got %v", typesOf(evs))
	}
	if evs[0].Payload["source"] != "os" {
		t.Errorf("payload must say which monitor saw it: %+v", evs[0].Payload)
	}
	if evs[0].Payload["title"] != "答案.txt - 记事本" {
		t.Errorf("window title is the evidence; lost it: %+v", evs[0].Payload)
	}

	// Sitting there longer is still one event.
	h.advance(2 * fgDwell)
	h.wantTypes()
}

func TestForegroundRepeatIsSuppressed(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.fg = fgInfo{PID: 60, Process: "notepad.exe"}
	h.advance(fgDwell + time.Second)
	h.wantTypes(protocol.EvForegroundLost)

	// Alt-tab away and back, twice, inside the suppression window: the
	// invigilator learns nothing from the second and third reports.
	for i := 0; i < 2; i++ {
		h.probe.fg = fgInfo{PID: 7, Process: "devcpp.exe"}
		h.advance(2 * time.Second)
		h.probe.fg = fgInfo{PID: 60, Process: "notepad.exe"}
		h.advance(fgDwell + time.Second)
		h.wantTypes()
	}

	// Past the suppression window, a fresh episode is reported again.
	h.probe.fg = fgInfo{PID: 7, Process: "devcpp.exe"}
	h.advance(fgRepeatSuppress)
	h.drain()
	h.probe.fg = fgInfo{PID: 60, Process: "notepad.exe"}
	h.advance(fgDwell + time.Second)
	h.wantTypes(protocol.EvForegroundLost)
}

// ----------------------------------------------------------------- network

func TestOnlyPublicDestinationsAreReported(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.conns = []connInfo{
		{PID: 1, Process: "coderoom-client.exe", Remote: "192.168.1.10:8080"},
		{PID: 2, Process: "svchost.exe", Remote: "20.42.65.92:443"},     // Windows itself
		{PID: 3, Process: "devcpp.exe", Remote: "10.0.0.5:445"},         // LAN file share
		{PID: 4, Process: "something.exe", Remote: "100.80.1.1:443"},    // CGNAT: still inside
		{PID: 5, Process: "msedge.exe", Remote: "142.250.property:443"}, // unparseable
		{PID: 6, Process: "msedge.exe", Remote: "142.250.200.100:443"},  // the real thing
	}
	h.tick()
	evs := h.drain()
	if len(evs) != 1 {
		t.Fatalf("want exactly one external_network, got %v", typesOf(evs))
	}
	if evs[0].Payload["remote"] != "142.250.200.100:443" {
		t.Errorf("wrong connection reported: %+v", evs[0].Payload)
	}

	// The same destination again is not news.
	h.advance(3 * connPollInterval)
	h.wantTypes()
}

func TestIsExternalIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"142.250.200.100", true},
		{"2404:6800:4003::1", true},
		{"192.168.1.10", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"127.0.0.1", false},
		{"169.254.1.1", false},
		{"100.80.1.1", false}, // CGNAT
		{"224.0.0.251", false},
		{"::1", false},
		{"fe80::1", false},
		{"::ffff:192.168.1.5", false}, // IPv4-mapped private
		{"not-an-ip", false},
	} {
		if got := isExternalIP(tc.ip); got != tc.want {
			t.Errorf("isExternalIP(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

// -------------------------------------------------------------- policy gate

func TestDisabledChecksDoNothing(t *testing.T) {
	h := newHarness(t, protocol.Policy{}) // every monitor off
	h.probe.drv = []driveInfo{{Letter: "F:", Kind: "removable", Ready: true}}
	h.probe.procs = []procInfo{{PID: 1, Name: "chrome.exe"}}
	h.probe.fg = fgInfo{PID: 2, Process: "notepad.exe"}
	h.probe.conns = []connInfo{{PID: 3, Process: "msedge.exe", Remote: "8.8.8.8:443"}}
	h.advance(5 * fgDwell)
	h.wantTypes()
}

// Turning a check on mid-exam is the whole point of pulling policy, so the
// first observation after it is enabled must be a baseline and not a flood of
// alerts about a USB stick that has been in the machine since breakfast.
func TestEnablingACheckMidExamRebaselines(t *testing.T) {
	h := newHarness(t, protocol.Policy{})
	h.probe.drv = []driveInfo{{Letter: "F:", Kind: "removable", Ready: true}}
	h.advance(5 * time.Second)
	h.wantTypes()

	h.policy = monitorAll()
	h.advance(2 * drivePollInterval)
	h.wantTypes()

	h.probe.drv = append(h.probe.drv, driveInfo{Letter: "G:", Kind: "removable", Ready: true})
	h.advance(drivePollInterval)
	h.wantTypes(protocol.EvUSBInserted)
}

// A monitor the platform cannot run must say so. Silence from a broken check
// is indistinguishable from silence from an honest room, which is the one
// failure mode that makes a dashboard actively misleading.
func TestUnsupportedChecksAreReportedOnce(t *testing.T) {
	h := newHarness(t, monitorAll())
	h.probe.c = probeCaps{} // nothing supported, as on the macOS dev build
	h.advance(2 * time.Second)

	evs := h.drain()
	if len(evs) != 4 {
		t.Fatalf("want one degraded event per check, got %v", typesOf(evs))
	}
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.Type != protocol.EvMonitorDegraded {
			t.Fatalf("unexpected event %q", ev.Type)
		}
		seen[ev.Payload["check"].(string)] = true
	}
	for _, want := range []string{"usb", "process", "foreground", "network"} {
		if !seen[want] {
			t.Errorf("no degraded event for %q", want)
		}
	}

	// Not repeated every tick: it is a fact about the build, not an incident.
	h.advance(30 * time.Second)
	h.wantTypes()
}

func TestNormalizeProcName(t *testing.T) {
	for in, want := range map[string]string{
		"chrome.exe":                 "chrome",
		"Chrome.EXE":                 "chrome",
		`C:\Program Files\QQ\QQ.exe`: "qq",
		"/usr/bin/python3":           "python3",
		"  msedge  ":                 "msedge",
		"":                           "",
	} {
		if got := normalizeProcName(in); got != want {
			t.Errorf("normalizeProcName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestActiveMonitorsDescribesThePolicy(t *testing.T) {
	got := activeMonitors(protocol.DefaultPolicyFor(protocol.ExamTypeExam))
	want := []string{"U盘", "进程", "窗口", "粘贴"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if n := len(activeMonitors(protocol.Policy{})); n != 0 {
		t.Errorf("an all-off policy must describe nothing, got %d entries", n)
	}
}
