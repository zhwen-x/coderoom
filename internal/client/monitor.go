package client

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"coderoom/internal/protocol"
)

// The behaviour monitor (M3). It detects and reports; it never blocks. Four
// checks run on their own cadences against the OS, and every one of them is
// governed by the policy the client pulled from the server, so the teacher can
// turn a check on mid-exam and the machines pick it up on their next heartbeat
// without anyone touching them.
//
// Noise is the design problem here, not detection. A check that fires on
// ordinary behaviour trains the invigilator to ignore the dashboard, which is
// worse than not having the check: the room looks supervised and is not. So
// every rule below errs towards silence, and the suppression is explained
// where it happens.
const (
	drivePollInterval = 1 * time.Second
	fgPollInterval    = 1 * time.Second
	procPollInterval  = 3 * time.Second
	connPollInterval  = 5 * time.Second

	// A foreground window must hold focus this long before it is reported.
	// Below it sit all the switches the workflow itself causes: clicking the
	// taskbar, a compile finishing, the student's own program opening a
	// console for two seconds.
	fgDwell = 20 * time.Second

	// The same foreground process is not reported again inside this window,
	// so alt-tabbing between two windows yields one event, not forty.
	fgRepeatSuppress = 2 * time.Minute

	// Window titles can hold a whole document name; keep the evidence, drop
	// the essay.
	maxTitleRunes = 120
)

// driveInfo is one drive letter currently present on the machine. Ready is
// false for a letter that exists with no media behind it - an empty card
// reader slot, which Windows keeps assigned whether a card is in it or not.
type driveInfo struct {
	Letter string // "F:"
	Kind   string // removable | fixed | cdrom | remote | ramdisk | unknown
	Ready  bool
}

type procInfo struct {
	PID  uint32
	Name string // executable name only, e.g. "chrome.exe"
}

type fgInfo struct {
	PID     uint32
	Process string
	Title   string
}

type connInfo struct {
	PID     uint32
	Process string
	Remote  string // "ip:port"
}

// probeCaps says which checks this build can actually perform. A check the
// platform cannot do is reported to the teacher as degraded rather than
// quietly returning nothing - silence from a broken monitor is
// indistinguishable from silence from an honest room.
type probeCaps struct {
	Drives      bool
	Processes   bool
	Foreground  bool
	Connections bool
}

// sysProbe is the operating-system half of the monitor. Everything above it is
// portable and unit-tested with a fake; everything below it is Windows API
// calls in sysprobe_windows.go.
type sysProbe interface {
	caps() probeCaps
	drives() ([]driveInfo, error)
	processes() ([]procInfo, error)
	foreground() (fgInfo, error)
	connections() ([]connInfo, error)
}

// monitorDeps is everything the monitor needs from its surroundings, passed in
// rather than reached for, so the whole thing runs in a test on any platform.
type monitorDeps struct {
	probe sysProbe
	// policy is read on every tick: a policy change must take effect without
	// restarting anything, since that is the whole point of pulling policy.
	policy func() protocol.Policy
	emit   func(protocol.Event)
	// setForeground publishes the live foreground app for the heartbeat.
	setForeground func(string)
	// serverHost is excluded from the network check: talking to the judge
	// server is the client's job.
	serverHost func() string
	now        func() time.Time
	logf       func(string, ...any)
}

type monitor struct {
	d monitorDeps

	drives      map[string]driveInfo // usable drives, as last seen
	drivesKnown bool                 // the baseline has been taken
	badProcs    map[string]bool      // blacklisted names currently running
	fgProcess   string               // foreground process as last seen
	fgSince     time.Time            // when it took the foreground
	fgReported  bool                 // already reported this episode
	fgLastSent  map[string]time.Time // process -> last report, for suppression
	seenRemote  map[string]bool      // "process|ip", reported once per run
	degraded    map[string]bool      // check -> already told the teacher

	next map[string]time.Time
}

func newMonitor(d monitorDeps) *monitor {
	if d.now == nil {
		d.now = time.Now
	}
	if d.logf == nil {
		d.logf = func(string, ...any) {}
	}
	return &monitor{
		d:          d,
		drives:     map[string]driveInfo{},
		badProcs:   map[string]bool{},
		fgLastSent: map[string]time.Time{},
		seenRemote: map[string]bool{},
		degraded:   map[string]bool{},
		next:       map[string]time.Time{},
	}
}

// Run polls until ctx is cancelled. One goroutine for all four checks keeps
// the cost predictable and means a policy change is applied at one point.
func (m *monitor) Run(ctx context.Context) {
	// The first tick deliberately runs every check: whatever is already open
	// when the client starts is as interesting as what opens later.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

// tick runs whichever checks are due and enabled. Exported behaviour lives
// here rather than in Run so a test can step time by hand.
func (m *monitor) tick() {
	p := m.d.policy()
	caps := m.d.probe.caps()
	now := m.d.now()

	if m.due("drives", now, drivePollInterval, p.MonitorUSB) {
		if m.check("usb", caps.Drives) {
			m.checkDrives()
		}
	}
	if m.due("fg", now, fgPollInterval, p.MonitorForeground) {
		if m.check("foreground", caps.Foreground) {
			m.checkForeground(p, now)
		}
	}
	if m.due("procs", now, procPollInterval, p.MonitorProcess) {
		if m.check("process", caps.Processes) {
			m.checkProcesses(p)
		}
	}
	if m.due("conns", now, connPollInterval, p.MonitorNetwork) {
		if m.check("network", caps.Connections) {
			m.checkConnections(p)
		}
	}
}

// due reports whether a check should run now, and resets its state when the
// policy turns it off so that re-enabling it starts from a clean slate rather
// than from observations made minutes ago.
func (m *monitor) due(key string, now time.Time, every time.Duration, enabled bool) bool {
	if !enabled {
		delete(m.next, key)
		m.forget(key)
		return false
	}
	if at, ok := m.next[key]; ok && now.Before(at) {
		return false
	}
	m.next[key] = now.Add(every)
	return true
}

func (m *monitor) forget(key string) {
	switch key {
	case "drives":
		m.drives, m.drivesKnown = map[string]driveInfo{}, false
	case "procs":
		m.badProcs = map[string]bool{}
	case "fg":
		m.fgProcess, m.fgReported = "", false
		m.publishForeground("")
	case "conns":
		m.seenRemote = map[string]bool{}
	}
}

// check gates a capability, telling the teacher once if it is missing.
func (m *monitor) check(name string, ok bool) bool {
	if ok {
		return true
	}
	if !m.degraded[name] {
		m.degraded[name] = true
		m.emit(protocol.EvMonitorDegraded, protocol.SevWarn, map[string]any{
			"check":  name,
			"reason": "本机客户端不支持该监控项",
		})
	}
	return false
}

// canWatchForeground reports whether this build can see the foreground window
// at all, which decides whether the UI's own blur event is still needed.
func (m *monitor) canWatchForeground() bool { return m.d.probe.caps().Foreground }

func (m *monitor) publishForeground(name string) {
	if m.d.setForeground != nil {
		m.d.setForeground(name)
	}
}

func (m *monitor) emit(typ, sev string, payload map[string]any) {
	m.d.emit(protocol.Event{
		Type:       typ,
		Severity:   sev,
		ClientTime: m.d.now().UnixMilli(),
		Payload:    payload,
	})
}

// ------------------------------------------------------------------- drives

// checkDrives reports storage becoming usable, and ceasing to be.
//
// The rule is one transition, not a device taxonomy: a drive letter that
// becomes present *and* ready is an insertion, and one that stops being either
// is a removal. That single rule covers the three shapes this takes in a lab -
// a flash drive bringing a new letter with it, a card going into a reader that
// already owns a letter, and either coming back out.
//
// Asking "is this USB storage?" instead would be both harder and worse. A
// flash drive reports DRIVE_REMOVABLE but a USB hard disk reports DRIVE_FIXED,
// so telling them apart needs SetupAPI device enumeration - and watching
// usability catches the USB hard disk for free, since nobody mounts a new
// fixed disk in the middle of an exam. The drive kind still rides along in the
// payload as evidence, and sets the severity.
func (m *monitor) checkDrives() {
	list, err := m.d.probe.drives()
	if err != nil {
		m.d.logf("监控: 枚举驱动器失败: %v", err)
		return
	}
	usable := make(map[string]driveInfo, len(list))
	for _, d := range list {
		if d.Ready {
			usable[d.Letter] = d
		}
	}
	// The first observation is the baseline: whatever was already mounted when
	// the client started was not inserted during the exam.
	if !m.drivesKnown {
		m.drives, m.drivesKnown = usable, true
		return
	}
	for letter, d := range usable {
		if _, had := m.drives[letter]; had {
			continue
		}
		sev := protocol.SevWarn
		if d.Kind == "removable" || d.Kind == "cdrom" {
			sev = protocol.SevAlert
		}
		m.emit(protocol.EvUSBInserted, sev, map[string]any{
			"drive": letter, "kind": d.Kind,
		})
	}
	for letter, d := range m.drives {
		if _, still := usable[letter]; still {
			continue
		}
		m.emit(protocol.EvUSBRemoved, protocol.SevInfo, map[string]any{
			"drive": letter, "kind": d.Kind,
		})
	}
	m.drives = usable
}

// ---------------------------------------------------------------- processes

// checkProcesses reports blacklisted programs that are running at all, in the
// foreground or not: a browser left open behind the IDE is the same problem as
// one on screen.
func (m *monitor) checkProcesses(p protocol.Policy) {
	if len(p.ProcessBlacklist) == 0 {
		return
	}
	list, err := m.d.probe.processes()
	if err != nil {
		m.d.logf("监控: 枚举进程失败: %v", err)
		return
	}
	black := newNameSet(p.ProcessBlacklist)
	white := newNameSet(p.ProcessWhitelist)

	running := map[string]bool{}
	for _, pr := range list {
		key := normalizeProcName(pr.Name)
		if key == "" || white.has(key) || !black.has(key) {
			continue
		}
		running[key] = true
		if m.badProcs[key] {
			continue // already reported, still running: stay quiet
		}
		m.emit(protocol.EvProcBlacklisted, protocol.SevAlert, map[string]any{
			"process": pr.Name, "pid": pr.PID,
		})
	}
	// Dropping a name that has exited means a re-launch alerts again, which is
	// what an invigilator wants: closing and reopening a browser is not a
	// reason to stop reporting it.
	m.badProcs = running
}

// --------------------------------------------------------------- foreground

// checkForeground tracks what the student is looking at. Two outputs, for two
// different questions:
//
//   - the live foreground app, published for the heartbeat, answers "what is
//     on his screen right now" with no event-log cost at all;
//   - an event, once a non-whitelisted window has held focus for fgDwell,
//     answers "where has he been" after the fact.
//
// The dwell threshold is what makes this usable. Students are expected to
// spend most of the exam in the IDE (whitelisted) and to tab into CodeRoom to
// paste; a window that is neither, held for twenty seconds, is a choice.
func (m *monitor) checkForeground(p protocol.Policy, now time.Time) {
	fg, err := m.d.probe.foreground()
	if err != nil {
		m.d.logf("监控: 读取前台窗口失败: %v", err)
		return
	}
	key := normalizeProcName(fg.Process)
	if key != m.fgProcess {
		m.fgProcess, m.fgSince, m.fgReported = key, now, false
	}
	m.publishForeground(fg.Process)

	if m.fgReported || key == "" {
		return
	}
	// Our own window is the one place the student is supposed to be. The
	// WebView2 host is listed too: the top-level window belongs to this
	// process, but a child window of the embedded browser can surface instead.
	if key == ownProcess || key == "msedgewebview2" {
		return
	}
	if newNameSet(p.ProcessWhitelist).has(key) {
		return
	}
	if now.Sub(m.fgSince) < fgDwell {
		return
	}
	if last, ok := m.fgLastSent[key]; ok && now.Sub(last) < fgRepeatSuppress {
		m.fgReported = true // suppressed, but this episode is done with
		return
	}
	m.fgReported = true
	m.fgLastSent[key] = now
	m.emit(protocol.EvForegroundLost, protocol.SevWarn, map[string]any{
		"source": "os", "process": fg.Process,
		"title": truncateRunes(fg.Title, maxTitleRunes),
		"pid":   fg.PID, "dwell_ms": now.Sub(m.fgSince).Milliseconds(),
	})
}

// ---------------------------------------------------------------- network

// systemNetProcesses talk to the internet as a matter of course - Windows
// Update, telemetry, Defender, time sync, the WebView our own UI runs in. They
// are filtered before anything is reported, because fifty lines of svchost per
// minute would make the one line that matters unfindable.
//
// A student who renames chrome.exe to svchost.exe lands in here and escapes
// the network check. That hole is accepted: M3 detects rather than enforces,
// and the process blacklist plus the foreground title still show the window.
var systemNetProcesses = newNameSet([]string{
	"svchost", "system", "lsass", "services", "wininit", "winlogon",
	"smss", "csrss", "sihost", "ctfmon", "taskhostw", "dllhost", "wmiprvse",
	"runtimebroker", "backgroundtaskhost", "applicationframehost",
	"searchapp", "searchui", "searchindexer", "startmenuexperiencehost",
	"shellexperiencehost", "textinputhost", "lockapp", "systemsettings",
	"mousocoreworker", "usocoreworker", "wuauclt", "trustedinstaller",
	"msmpeng", "nissrv", "mpdefendercoreservice", "securityhealthservice",
	"smartscreen", "onedrive", "officeclicktorun",
	"msedgewebview2", // the host process of this client's own UI
	"coderoom-client",
})

// checkConnections reports TCP connections leaving the campus network.
//
// This is the noisiest and least reliable of the four checks, which is why it
// is off by default: Windows is chatty, the system-process filter is a
// blacklist of names rather than a real identity check, and a lab whose
// gateway NATs everything may produce surprises. Turned on deliberately, with
// a teacher reading it, it answers one useful question: is anything on this
// machine talking to the outside world.
func (m *monitor) checkConnections(p protocol.Policy) {
	list, err := m.d.probe.connections()
	if err != nil {
		m.d.logf("监控: 枚举网络连接失败: %v", err)
		return
	}
	white := newNameSet(p.ProcessWhitelist)
	serverIP := hostOnly(m.d.serverHost)

	for _, c := range list {
		key := normalizeProcName(c.Process)
		if white.has(key) || systemNetProcesses.has(key) {
			continue
		}
		host, _, err := net.SplitHostPort(c.Remote)
		if err != nil {
			continue
		}
		if host == serverIP || !isExternalIP(host) {
			continue
		}
		dedup := key + "|" + host
		if m.seenRemote[dedup] {
			continue
		}
		// Bound the map: a machine with a genuinely busy connection list must
		// not grow the client's memory without limit.
		if len(m.seenRemote) < 512 {
			m.seenRemote[dedup] = true
		}
		m.emit(protocol.EvExternalNet, protocol.SevWarn, map[string]any{
			"process": c.Process, "pid": c.PID, "remote": c.Remote,
		})
	}
}

// cgnatPrefix is carrier-grade NAT space. Some campus networks hand these out,
// so a machine talking to one has not necessarily left the institution.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isExternalIP reports whether an address is outside the local network. Only
// routable public addresses count: everything private, loopback, link-local,
// multicast or CGNAT is treated as inside.
func isExternalIP(s string) bool {
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	switch {
	case !addr.IsValid(), addr.IsUnspecified(), addr.IsLoopback(),
		addr.IsPrivate(), addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast(),
		addr.IsMulticast(), addr.IsInterfaceLocalMulticast():
		return false
	}
	if addr.Is4() && cgnatPrefix.Contains(addr) {
		return false
	}
	return true
}

func hostOnly(f func() string) string {
	if f == nil {
		return ""
	}
	addr := f()
	if addr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// ------------------------------------------------------------------ helpers

// nameSet matches executable names the way a teacher types them: case does not
// matter and the ".exe" is optional, so "chrome", "Chrome" and "chrome.exe"
// are one entry. Matching is on the whole name, never a substring, so "QQ"
// does not fire on QQBrowser.exe.
type nameSet map[string]bool

func newNameSet(names []string) nameSet {
	s := make(nameSet, len(names))
	for _, n := range names {
		if k := normalizeProcName(n); k != "" {
			s[k] = true
		}
	}
	return s
}

func (s nameSet) has(normalized string) bool { return s[normalized] }

func normalizeProcName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	// A full path may arrive from the OS side; only the leaf is compared.
	if i := strings.LastIndexAny(n, `\/`); i >= 0 {
		n = n[i+1:]
	}
	return strings.TrimSuffix(n, ".exe")
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// activeMonitors names the checks currently in force, for the student-facing
// status line. Students are told what is watched: an exam where the rules are
// visible produces fewer disputes afterwards, and a hidden monitor deters
// nobody.
func activeMonitors(p protocol.Policy) []string {
	var out []string
	if p.MonitorUSB {
		out = append(out, "U盘")
	}
	if p.MonitorProcess {
		out = append(out, "进程")
	}
	if p.MonitorForeground {
		out = append(out, "窗口")
	}
	if p.MonitorNetwork {
		out = append(out, "外网")
	}
	if p.MonitorPaste {
		out = append(out, "粘贴")
	}
	return out
}

// ownProcess is this executable's name, normalised once at startup so the
// foreground check can recognise its own window without a syscall per tick.
var ownProcess = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "coderoom-client"
	}
	return normalizeProcName(filepath.Base(exe))
}()
