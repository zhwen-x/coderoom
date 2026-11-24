// Package protocol defines the wire types shared between the CodeRoom client
// (student machine) and server (teacher machine).
//
// Two invariants hold across every client-facing response:
//
//   - ServerTime is authoritative. The client never trusts its own clock for
//     countdowns or exam boundaries; local time is only recorded as evidence.
//   - PolicyVersion lets the client detect a policy change on any round trip,
//     so control policy is *pulled* by the client rather than pushed by the
//     server. A student machine that boots late or reboots mid-exam therefore
//     converges on the current policy as soon as its client starts.
package protocol

import "encoding/json"

// Judge verdicts.
const (
	VerdictPending = "PD"  // queued, not judged yet
	VerdictAC      = "AC"  // accepted
	VerdictWA      = "WA"  // wrong answer
	VerdictTLE     = "TLE" // time limit exceeded
	VerdictMLE     = "MLE" // memory limit exceeded
	VerdictRE      = "RE"  // runtime error (non-zero exit / signal)
	VerdictCE      = "CE"  // compile error
	VerdictOLE     = "OLE" // output limit exceeded
	VerdictSE      = "SE"  // system error on the judge side
)

// Exam types.
const (
	ExamTypePractice = "practice"
	ExamTypeExam     = "exam"
)

// Exam lifecycle states.
const (
	ExamStateDraft     = "draft"
	ExamStatePublished = "published"
	ExamStateRunning   = "running"
	ExamStateEnded     = "ended"
	ExamStateJudging   = "judging"
	ExamStateGraded    = "graded"
)

// Submission kinds. Snapshots are collected automatically on a timer; finals
// are the student's deliberate submission.
const (
	KindSnapshot = "snapshot"
	KindFinal    = "final"
)

// Languages supported in M0/M1.
const (
	LangCPP    = "cpp"
	LangC      = "c"
	LangPython = "python"
)

// Event severities.
const (
	SevInfo  = "info"
	SevWarn  = "warn"
	SevAlert = "alert"
)

// Behaviour event types. Declared in full now so the client, server and
// invigilation dashboard agree on names from the start, even though only the
// editor-side ones are emitted before M3.
const (
	EvUSBInserted     = "usb_inserted"
	EvUSBRemoved      = "usb_removed"
	EvProcBlacklisted = "process_blacklisted"
	EvForegroundLost  = "foreground_lost"
	EvPasteLarge      = "paste_large"
	EvPaste           = "paste"
	EvExternalNet     = "external_network"
	EvClockSkew       = "clock_skew"
	EvLoginConflict   = "login_conflict"
	EvHeartbeatLost   = "heartbeat_lost" // server-side only
	EvClientStarted   = "client_started"
	EvClientExiting   = "client_exiting"

	// EvMonitorDegraded is raised when the policy asks for a check the client
	// cannot perform. Without it, a monitor that silently fails looks exactly
	// like a room where nobody cheated.
	EvMonitorDegraded = "monitor_degraded"

	// Enforcement (M4). A teacher has to be able to tell, per machine,
	// whether the lock they asked for is actually in force - and must be told
	// when one was left behind by a crash, because that is a machine somebody
	// may have found unusable.
	EvGuardApplied     = "guard_applied"
	EvGuardReleased    = "guard_released"
	EvGuardFailed      = "guard_failed"
	EvGuardUnavailable = "guard_unavailable"
	EvGuardRecovered   = "guard_recovered"
)

// Command types on the server -> client control channel.
const (
	// CmdReleaseGuard is the teacher's emergency escape: drop every lock on
	// this machine now, whatever the policy says. It exists because the one
	// failure this system must never have is a room full of machines nobody
	// can use and no way to intervene.
	CmdReleaseGuard = "release_guard"
)

// Envelope is embedded in every client-facing response.
type Envelope struct {
	ServerTime    int64 `json:"server_time"`    // unix milliseconds
	PolicyVersion int   `json:"policy_version"` // bump => client re-pulls policy
}

// Policy is the full control policy for a client. Monitoring fields land in
// M3, enforcement fields in M4; both are declared now so the transport never
// has to change.
type Policy struct {
	Version              int `json:"version"`
	SnapshotIntervalSec  int `json:"snapshot_interval_sec"`
	HeartbeatIntervalSec int `json:"heartbeat_interval_sec"`

	// Monitoring (M3) - detect and report, never block.
	MonitorUSB        bool     `json:"monitor_usb"`
	MonitorProcess    bool     `json:"monitor_process"`
	MonitorForeground bool     `json:"monitor_foreground"`
	MonitorPaste      bool     `json:"monitor_paste"`
	MonitorNetwork    bool     `json:"monitor_network"`
	ProcessBlacklist  []string `json:"process_blacklist"`
	ProcessWhitelist  []string `json:"process_whitelist"`

	// Enforcement (M4) - requires admin rights and a watchdog that can undo
	// every change. Always false before M4.
	BlockUSBStorage bool     `json:"block_usb_storage"`
	BlockInternet   bool     `json:"block_internet"`
	AllowedHosts    []string `json:"allowed_hosts"`
}

// DefaultProcessBlacklist is what a new exam watches for: browsers, instant
// messengers and remote-control tools - the three channels a student uses to
// reach outside help. Matching is on the executable's own name, lower-cased,
// with a trailing ".exe" optional on either side; it is never a substring
// match, so "QQ" does not fire on QQBrowser.exe (listed separately).
//
// Renaming an executable defeats this list. That is accepted: M3 detects, it
// does not enforce, and a student who renames chrome.exe still shows up in the
// foreground monitor by window title.
var DefaultProcessBlacklist = []string{
	// Browsers.
	"chrome", "msedge", "firefox", "iexplore", "opera", "brave",
	"360se", "360chrome", "QQBrowser", "SogouExplorer", "TheWorld", "UCBrowser",
	// Instant messengers.
	"QQ", "TIM", "WeChat", "WeChatAppEx", "DingTalk", "Feishu", "Telegram",
	// Remote control and screen sharing.
	"ToDesk", "SunloginClient", "TeamViewer", "AnyDesk", "RustDesk", "mstsc",
	"AweSun", "RayLink", "vncviewer",
}

// DefaultProcessWhitelist is never reported by the foreground or network
// monitors, and overrides the blacklist if a name appears on both.
//
// Most of it is the intended workflow rather than an exception to it: students
// write code in the lab's own IDE and paste it into CodeRoom, so the IDE being
// in the foreground is the normal state, not an anomaly. Alerting on it would
// bury every real signal.
var DefaultProcessWhitelist = []string{
	// JiYu classroom, the lab's existing control software. Its broadcast
	// teaching mode seizes the foreground on every machine at once, which
	// would otherwise alert for the whole room in the same second.
	"StudentMain.exe", "TopDomain.exe",
	// The lab's IDEs: Visual Studio, Dev-C++, PyCharm, IDLE.
	"devenv.exe", "devcpp.exe", "pycharm64.exe", "pycharm.exe",
	"idle.exe", "pythonw.exe", "python.exe", "codeblocks.exe", "Code.exe",
	// Running one's own program, and the consoles that come with it.
	"cmd.exe", "conhost.exe", "powershell.exe", "WindowsTerminal.exe",
}

// DefaultPolicy is the baseline every policy starts from.
func DefaultPolicy() Policy {
	return Policy{
		Version:              1,
		SnapshotIntervalSec:  30,
		HeartbeatIntervalSec: 10,
		MonitorPaste:         true,
		ProcessWhitelist:     append([]string(nil), DefaultProcessWhitelist...),
	}
}

// DefaultPolicyFor is what a newly created exam gets. The two exam types want
// different defaults: during practice a student looking something up in a
// browser is studying, and alerting on it would bury the dashboard in noise
// that nobody acts on. An exam flips those checks on.
//
// Network monitoring stays off even for exams. It is the noisiest check by far
// (Windows itself talks to the internet constantly) and wants a teacher who
// has decided to read its output - see monitor.go.
func DefaultPolicyFor(examType string) Policy {
	p := DefaultPolicy()
	if examType != ExamTypeExam {
		return p
	}
	p.MonitorUSB = true
	p.MonitorProcess = true
	p.MonitorForeground = true
	p.ProcessBlacklist = append([]string(nil), DefaultProcessBlacklist...)
	return p
}

// MachineInfo is recorded at login so the teacher can spot the same student
// number appearing on two machines.
type MachineInfo struct {
	Hostname      string `json:"hostname"`
	MAC           string `json:"mac"`
	OS            string `json:"os"`
	ClientVersion string `json:"client_version"`
}

// ExamBrief is the exam as advertised to a not-yet-logged-in client.
type ExamBrief struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	State            string `json:"state"`
	RosterRequired   bool   `json:"roster_required"`
	PasscodeRequired bool   `json:"passcode_required"`
	StartAt          *int64 `json:"start_at"`
	EndAt            *int64 `json:"end_at"`
	DurationMin      int    `json:"duration_min"`
	AllowRejudge     bool   `json:"allow_rejudge"` // practice: may resubmit
	ShowResults      bool   `json:"show_results"`  // practice: instant feedback
}

// ExamListResponse is served unauthenticated so the login screen can offer a
// choice of open sessions.
type ExamListResponse struct {
	Envelope
	Exams []ExamBrief `json:"exams"`
}

type LoginRequest struct {
	ExamID     int64       `json:"exam_id"`
	StudentNo  string      `json:"student_no"`
	Name       string      `json:"name"`
	Passcode   string      `json:"passcode"`
	Machine    MachineInfo `json:"machine"`
	ClientTime int64       `json:"client_time"`
}

type LoginResponse struct {
	Envelope
	Token     string    `json:"token"`
	SessionID int64     `json:"session_id"`
	StudentNo string    `json:"student_no"`
	Name      string    `json:"name"`
	Class     string    `json:"class"`
	Exam      ExamBrief `json:"exam"`
	Policy    Policy    `json:"policy"`
}

type Sample struct {
	Input  string `json:"input"`
	Output string `json:"output"`
	Note   string `json:"note"`
}

// Problem is the student-visible view of a problem. Test data never crosses
// this boundary.
type Problem struct {
	ID            int64    `json:"id"`
	Title         string   `json:"title"`
	DescriptionMD string   `json:"description_md"`
	InputDesc     string   `json:"input_desc"`
	OutputDesc    string   `json:"output_desc"`
	Samples       []Sample `json:"samples"`
	TimeLimitMS   int      `json:"time_limit_ms"`
	MemoryLimitMB int      `json:"memory_limit_mb"`
	Languages     []string `json:"languages"`
}

type PaperItem struct {
	Index     int      `json:"index"`
	ProblemID int64    `json:"problem_id"`
	Score     int      `json:"score"`
	Problem   *Problem `json:"problem"` // nil while the paper is locked
}

type PaperResponse struct {
	Envelope
	PaperID  int64       `json:"paper_id"`
	Locked   bool        `json:"locked"` // exam not open yet: bodies withheld
	Reason   string      `json:"reason"`
	Items    []PaperItem `json:"items"`
	Deadline *int64      `json:"deadline"`
	Drafts   []Draft     `json:"drafts"` // last known code, for crash recovery
}

// Draft lets a restarted client recover whatever the server last accepted.
type Draft struct {
	ProblemID int64  `json:"problem_id"`
	Language  string `json:"language"`
	Code      string `json:"code"`
	UpdatedAt int64  `json:"updated_at"`
}

type SnapshotItem struct {
	ProblemID int64  `json:"problem_id"`
	Language  string `json:"language"`
	Code      string `json:"code"`
	Hash      string `json:"hash"` // sha256 hex, lets the server skip no-ops
}

type SnapshotRequest struct {
	Items      []SnapshotItem `json:"items"`
	ClientTime int64          `json:"client_time"`
}

type SnapshotResponse struct {
	Envelope
	Stored  []int64 `json:"stored"`
	Skipped []int64 `json:"skipped"`
}

type SubmitRequest struct {
	ProblemID  int64  `json:"problem_id"`
	Language   string `json:"language"`
	Code       string `json:"code"`
	Hash       string `json:"hash"`
	ClientTime int64  `json:"client_time"`
}

type SubmitResponse struct {
	Envelope
	SubmissionID int64  `json:"submission_id"`
	Queued       bool   `json:"queued"`
	Message      string `json:"message"`
}

type HeartbeatRequest struct {
	ClientTime    int64 `json:"client_time"`
	Focused       bool  `json:"focused"`
	PolicyVersion int   `json:"policy_version"`
	// Foreground is the window the student is actually looking at, as seen by
	// the OS monitor. Events record the moment of a switch; this records the
	// present, so the dashboard can answer "is he still in there?" without
	// replaying the event log.
	Foreground string `json:"foreground"`
	// Guard is what is actually enforced on this machine, which is not the
	// same thing as what the policy asked for. A teacher must be able to see
	// the difference before the exam, not deduce it from the results after.
	Guard *GuardReport `json:"guard,omitempty"`
}

// GuardReport is the client's account of enforcement on its own machine.
type GuardReport struct {
	Supported bool   `json:"supported"`
	Active    bool   `json:"active"`
	USB       bool   `json:"usb_blocked"`
	Internet  bool   `json:"internet_blocked"`
	Error     string `json:"error,omitempty"`
}

type HeartbeatResponse struct {
	Envelope
	ExamState string    `json:"exam_state"`
	Deadline  *int64    `json:"deadline"`
	Policy    *Policy   `json:"policy"` // non-nil when the client is out of date
	Commands  []Command `json:"commands"`
}

// Command is the server->client control channel. Unused in M0; M4 uses it for
// "release all control now" style interventions.
type Command struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type Event struct {
	Type       string         `json:"type"`
	Severity   string         `json:"severity"`
	ClientTime int64          `json:"client_time"`
	Payload    map[string]any `json:"payload"`
}

type EventsRequest struct {
	Events []Event `json:"events"`
}

type CaseResult struct {
	Index    int    `json:"index"`
	Verdict  string `json:"verdict"`
	Score    int    `json:"score"`
	MaxScore int    `json:"max_score"`
	TimeMS   int    `json:"time_ms"`
	MemoryKB int    `json:"memory_kb"`
	Message  string `json:"message"`
}

type SubmissionResult struct {
	SubmissionID int64        `json:"submission_id"`
	ProblemID    int64        `json:"problem_id"`
	Seq          int          `json:"seq"`
	Language     string       `json:"language"`
	Verdict      string       `json:"verdict"`
	Score        int          `json:"score"`
	MaxScore     int          `json:"max_score"`
	TimeMS       int          `json:"time_ms"`
	MemoryKB     int          `json:"memory_kb"`
	CompileLog   string       `json:"compile_log"`
	Cases        []CaseResult `json:"cases"`
	CreatedAt    int64        `json:"created_at"`
	JudgedAt     *int64       `json:"judged_at"`
}

type ResultsResponse struct {
	Envelope
	Visible bool               `json:"visible"` // exams hide results until released
	Reason  string             `json:"reason"`
	Results []SubmissionResult `json:"results"`
}

// ErrorResponse is returned with any non-2xx status.
type ErrorResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
}
