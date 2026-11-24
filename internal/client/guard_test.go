package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"coderoom/internal/protocol"
)

// fakeEnforcer stands in for the registry and the firewall, and records the
// order of every privileged action. Order is the point: the guarantee this
// milestone rests on is that nothing is changed before the record that
// reverses it exists on disk, and ordering bugs are invisible to a test that
// only checks the end state.
type fakeEnforcer struct {
	calls []string

	usbStart    uint32 // the machine's current USBSTOR value
	rules       map[string]bool
	tasks       map[string]bool
	unavailable string
	ownerPID    int

	failOn map[string]error
}

func newFakeEnforcer() *fakeEnforcer {
	return &fakeEnforcer{
		usbStart: 3,
		rules:    map[string]bool{},
		tasks:    map[string]bool{},
		failOn:   map[string]error{},
	}
}

func (f *fakeEnforcer) record(name string) error {
	f.calls = append(f.calls, name)
	return f.failOn[name]
}

func (f *fakeEnforcer) available() (bool, string) {
	if f.unavailable != "" {
		return false, f.unavailable
	}
	return true, ""
}

func (f *fakeEnforcer) snapshotUSB() (usbSnapshot, error) {
	if err := f.record("snapshotUSB"); err != nil {
		return usbSnapshot{}, err
	}
	return usbSnapshot{Start: f.usbStart}, nil
}

func (f *fakeEnforcer) disableUSBStorage() error {
	if err := f.record("disableUSB"); err != nil {
		return err
	}
	f.usbStart = 4
	return nil
}

func (f *fakeEnforcer) restoreUSB(s usbSnapshot) error {
	if err := f.record("restoreUSB"); err != nil {
		return err
	}
	f.usbStart = s.Start
	return nil
}

func (f *fakeEnforcer) snapshotFirewall() (firewallSnapshot, error) {
	if err := f.record("snapshotFirewall"); err != nil {
		return firewallSnapshot{}, err
	}
	return firewallSnapshot{Rules: []string{"v4", "v6"}}, nil
}

func (f *fakeEnforcer) blockInternet([]string) error {
	if err := f.record("blockInternet"); err != nil {
		return err
	}
	f.rules["v4"], f.rules["v6"] = true, true
	return nil
}

func (f *fakeEnforcer) restoreFirewall(s firewallSnapshot) error {
	if err := f.record("restoreFirewall"); err != nil {
		return err
	}
	for _, r := range s.Rules {
		delete(f.rules, r)
	}
	return nil
}

func (f *fakeEnforcer) installRecovery(name string, ownerPID int, bootTask bool) error {
	f.ownerPID = ownerPID
	if err := f.record("installRecovery"); err != nil {
		return err
	}
	if bootTask {
		f.tasks[name] = true
	} else {
		delete(f.tasks, name)
	}
	f.tasks[recoveryTickName] = true
	return nil
}

func (f *fakeEnforcer) removeRecovery(name string) error {
	if err := f.record("removeRecovery"); err != nil {
		return err
	}
	// Both names are always removed, mirroring the Windows enforcer: a task
	// left from a run under a different setting must not survive.
	delete(f.tasks, name)
	delete(f.tasks, recoveryTickName)
	return nil
}

// locked reports whether the machine is in any way still constrained.
func (f *fakeEnforcer) locked() bool {
	return f.usbStart != 3 || len(f.rules) > 0 || len(f.tasks) > 0
}

type guardHarness struct {
	t      *testing.T
	g      *Guard
	enf    *fakeEnforcer
	path   string
	events []protocol.Event
	logs   []string
}

func newGuardHarness(t *testing.T) *guardHarness {
	t.Helper()
	h := &guardHarness{t: t, enf: newFakeEnforcer()}
	h.path = filepath.Join(t.TempDir(), "guard", "guard.json")
	h.g = newGuard(guardDeps{
		enf:      h.enf,
		jrnlPath: h.path,
		emit:     func(ev protocol.Event) { h.events = append(h.events, ev) },
		logf:     func(f string, a ...any) { h.logs = append(h.logs, f) },
		now:      func() time.Time { return time.Unix(1700000000, 0) },
	})
	return h
}

func (h *guardHarness) journalExists() bool {
	_, err := os.Stat(h.path)
	return err == nil
}

func (h *guardHarness) sawEvent(typ string) bool {
	for _, ev := range h.events {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

func bothBlocked() protocol.Policy {
	p := protocol.DefaultPolicyFor(protocol.ExamTypeExam)
	p.BlockUSBStorage = true
	p.BlockInternet = true
	return p
}

func someInfo() applyInfo {
	return applyInfo{ExamID: 7, ExamName: "期末上机", StudentNo: "2301"}
}

// The ordering guarantee the whole milestone rests on: by the time anything on
// the machine has changed, both the record of how to undo it and the mechanism
// that will replay that record without this process already exist. Reversed, a
// power cut in the gap leaves a locked machine with nothing on it that knows
// how to unlock.
func TestNothingIsChangedBeforeRecoveryExists(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}

	order := strings.Join(h.enf.calls, ",")
	firstChange := -1
	for i, c := range h.enf.calls {
		if c == "disableUSB" || c == "blockInternet" {
			firstChange = i
			break
		}
	}
	if firstChange < 0 {
		t.Fatalf("nothing was applied at all: %s", order)
	}
	recovery := -1
	for i, c := range h.enf.calls {
		if c == "installRecovery" {
			recovery = i
		}
	}
	if recovery > firstChange {
		t.Errorf("boot recovery installed after the machine was changed: %s", order)
	}
	// And the journal has to be on disk by then too, with the original value
	// in it rather than the one we just wrote.
	raw, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatalf("no journal on disk after apply: %v", err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	if j.USB == nil || j.USB.Start != 3 {
		t.Errorf("journal did not capture the original USB value: %+v", j.USB)
	}
	if j.Firewall == nil || len(j.Firewall.Rules) != 2 {
		t.Errorf("journal did not capture the firewall rules: %+v", j.Firewall)
	}
	if j.ExamID != 7 || j.StudentNo != "2301" {
		t.Errorf("journal lost the session it belongs to: %+v", j)
	}
}

func TestApplyThenReleaseLeavesNothingBehind(t *testing.T) {
	h := newGuardHarness(t)
	h.enf.usbStart = 3
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	if h.enf.usbStart != 4 || len(h.enf.rules) != 2 || len(h.enf.tasks) != 2 {
		t.Fatalf("policy not applied: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}

	if err := h.g.Release("考试结束"); err != nil {
		t.Fatal(err)
	}
	if h.enf.locked() {
		t.Errorf("machine still constrained: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
	if h.journalExists() {
		t.Error("journal left on disk after a clean release")
	}
	if !h.sawEvent(protocol.EvGuardApplied) || !h.sawEvent(protocol.EvGuardReleased) {
		t.Error("the teacher was not told the machine was locked and unlocked")
	}
}

// A machine whose administrator had already disabled USB storage must stay
// that way. Restoring to a hard-coded "enabled" would quietly undo their
// decision on every machine in the room.
func TestReleaseRestoresWhatWasThereNotADefault(t *testing.T) {
	h := newGuardHarness(t)
	h.enf.usbStart = 4 // already disabled by the lab's own policy

	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	if err := h.g.Release("考试结束"); err != nil {
		t.Fatal(err)
	}
	if h.enf.usbStart != 4 {
		t.Errorf("USB start restored to %d, want the original 4", h.enf.usbStart)
	}
}

// Every exit path calls Release without first working out whether it needs to.
func TestReleaseIsIdempotentAndSafeWhenNothingIsApplied(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Release("退出"); err != nil {
		t.Fatalf("release with nothing applied returned %v", err)
	}
	if len(h.enf.calls) != 0 {
		t.Errorf("release touched the machine with nothing applied: %v", h.enf.calls)
	}

	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := h.g.Release("重复调用"); err != nil {
			t.Fatalf("release #%d: %v", i+1, err)
		}
	}
	if h.enf.locked() {
		t.Error("machine constrained after repeated releases")
	}
}

// A machine must never be left half-locked because one step failed.
func TestApplyRollsBackWhenAStepFails(t *testing.T) {
	h := newGuardHarness(t)
	h.enf.failOn["blockInternet"] = errors.New("netsh 挂了")

	err := h.g.Apply(bothBlocked(), someInfo())
	if err == nil {
		t.Fatal("apply reported success despite a failed step")
	}
	if h.enf.locked() {
		t.Errorf("machine left constrained after a failed apply: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
	if h.journalExists() {
		t.Error("journal left behind after a rolled-back apply")
	}
	if !h.sawEvent(protocol.EvGuardFailed) {
		t.Error("a failed lock was not reported to the teacher")
	}
}

// If the very first privileged read fails, nothing may be written or changed:
// there is no point installing a recovery mechanism for a lock that was never
// applied.
func TestApplyTouchesNothingIfTheFirstReadFails(t *testing.T) {
	h := newGuardHarness(t)
	h.enf.failOn["snapshotUSB"] = errors.New("注册表被锁")

	if err := h.g.Apply(bothBlocked(), someInfo()); err == nil {
		t.Fatal("apply reported success")
	}
	if h.journalExists() {
		t.Error("journal written despite nothing being applied")
	}
	if h.enf.locked() {
		t.Error("machine changed despite the snapshot failing")
	}
}

// A client with no administrator rights must refuse loudly rather than appear
// to lock a machine it has no power over.
func TestUnavailableEnforcementChangesNothingAndAlerts(t *testing.T) {
	h := newGuardHarness(t)
	h.enf.unavailable = "没有管理员权限"

	if err := h.g.Apply(bothBlocked(), someInfo()); err == nil {
		t.Fatal("apply claimed success with no way to enforce")
	}
	if len(h.enf.calls) != 0 {
		t.Errorf("machine touched despite enforcement being unavailable: %v", h.enf.calls)
	}
	if h.journalExists() {
		t.Error("journal written despite enforcement being unavailable")
	}
	if !h.sawEvent(protocol.EvGuardUnavailable) {
		t.Fatal("teacher not told the room is not actually locked down")
	}
	// Repeating this on every heartbeat would bury the dashboard.
	before := len(h.events)
	_ = h.g.Apply(bothBlocked(), someInfo())
	_ = h.g.Apply(bothBlocked(), someInfo())
	if len(h.events) != before {
		t.Errorf("standing condition re-reported %d more times", len(h.events)-before)
	}
}

// The crash case: a journal on disk from a run that never got to release.
// Startup restores it unconditionally rather than asking whether the lock is
// still legitimate, because answering that needs the server - which is exactly
// what may be unreachable.
func TestSelfCheckRestoresALeftoverLock(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}

	// Simulate the process dying: the journal and the machine state survive,
	// the in-memory guard does not.
	fresh := newGuard(guardDeps{
		enf: h.enf, jrnlPath: h.path,
		emit: func(ev protocol.Event) { h.events = append(h.events, ev) },
		logf: func(f string, a ...any) { h.logs = append(h.logs, f) },
		now:  func() time.Time { return time.Unix(1700003600, 0) },
	})
	if !h.enf.locked() {
		t.Fatal("precondition: the machine should still be locked")
	}

	fresh.SelfCheck()

	if h.enf.locked() {
		t.Errorf("startup left the machine locked: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
	if h.journalExists() {
		t.Error("journal survived a successful self-check")
	}
	if !h.sawEvent(protocol.EvGuardRecovered) {
		t.Error("a machine found locked after a crash was not reported")
	}
}

func TestSelfCheckDoesNothingWhenNoLockWasLeft(t *testing.T) {
	h := newGuardHarness(t)
	h.g.SelfCheck()
	if len(h.enf.calls) != 0 {
		t.Errorf("self-check touched the machine with no journal: %v", h.enf.calls)
	}
	if len(h.events) != 0 {
		t.Errorf("self-check raised events with nothing to recover: %v", h.events)
	}
}

// A journal that cannot be read is the worst case: something was changed and
// the record of what is unreadable. Deleting it would destroy the only
// evidence; the machine has to be flagged for a human instead.
func TestCorruptJournalIsKeptAndReported(t *testing.T) {
	h := newGuardHarness(t)
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.g.SelfCheck()

	if !h.journalExists() {
		t.Fatal("a corrupt journal was deleted; the evidence is gone")
	}
	if st := h.g.Status(); st.LastError == "" {
		t.Error("corrupt journal did not surface as an error")
	}
	joined := strings.Join(h.logs, " ")
	if !strings.Contains(joined, "手动检查") {
		t.Errorf("log does not tell anyone to check the machine: %v", h.logs)
	}
}

// When a release fails the journal must stay, so the watchdog, the next
// launch, or the boot task tries again. Deleting it would strand the machine.
func TestFailedReleaseKeepsTheJournalForTheNextAttempt(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	h.enf.failOn["restoreUSB"] = errors.New("注册表写入失败")

	if err := h.g.Release("考试结束"); err == nil {
		t.Fatal("release reported success despite failing")
	}
	if !h.journalExists() {
		t.Fatal("journal deleted after a failed release; nothing will retry")
	}

	// The next attempt succeeds and clears everything.
	delete(h.enf.failOn, "restoreUSB")
	if err := h.g.Release("重试"); err != nil {
		t.Fatal(err)
	}
	if h.enf.locked() || h.journalExists() {
		t.Error("retry did not finish the job")
	}
}

// One failing step must not stop the others: a machine with its network back
// but its USB still disabled is better than one that stopped at the first
// error and left both.
func TestReleaseContinuesPastAFailedStep(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	h.enf.failOn["restoreFirewall"] = errors.New("netsh 挂了")

	_ = h.g.Release("考试结束")

	if h.enf.usbStart != 3 {
		t.Errorf("USB not restored after an earlier step failed: %d", h.enf.usbStart)
	}
	if len(h.enf.tasks) != 0 {
		t.Error("recovery task not removed after an earlier step failed")
	}
}

// Turning one control off mid-exam must not leave the other in a state the
// journal no longer describes.
func TestPolicyChangeReleasesBeforeReapplying(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}

	usbOnly := bothBlocked()
	usbOnly.BlockInternet = false
	if err := h.g.Apply(usbOnly, someInfo()); err != nil {
		t.Fatal(err)
	}

	if len(h.enf.rules) != 0 {
		t.Errorf("firewall rules survived a policy that no longer wants them: %v", h.enf.rules)
	}
	if h.enf.usbStart != 4 {
		t.Errorf("USB should still be blocked: %d", h.enf.usbStart)
	}
	j, err := h.g.readJournal()
	if err != nil {
		t.Fatal(err)
	}
	if j.Firewall != nil {
		t.Error("journal still claims firewall rules that were removed")
	}
	if j.USB == nil || j.USB.Start != 3 {
		t.Errorf("journal lost the original USB value across the change: %+v", j.USB)
	}
}

// Re-applying the same policy on every heartbeat must be free.
func TestApplyIsANoOpWhenAlreadyHoldingTheSameControls(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	before := len(h.enf.calls)
	for i := 0; i < 5; i++ {
		if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.enf.calls) != before {
		t.Errorf("re-applying did %d extra privileged calls", len(h.enf.calls)-before)
	}
}

// A policy with nothing to enforce releases rather than being ignored: this is
// the path that unlocks a room when the teacher turns enforcement off.
func TestEmptyPolicyReleases(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	if err := h.g.Apply(protocol.DefaultPolicyFor(protocol.ExamTypeExam), someInfo()); err != nil {
		t.Fatal(err)
	}
	if h.enf.locked() {
		t.Errorf("turning enforcement off did not unlock: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
}

// The recovery task repeats every minute during an exam, so it must be able to
// tell "the client is working normally" from "the client is gone". Getting
// this backwards would either unlock every machine mid-exam or leave a
// tree-killed one locked until somebody rebooted it.
func TestRecoveryTaskLeavesALiveClientAlone(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	j, err := h.g.readJournal()
	if err != nil {
		t.Fatal(err)
	}
	if j.OwnerPID != os.Getpid() {
		t.Fatalf("journal owner pid = %d, want this process %d", j.OwnerPID, os.Getpid())
	}

	// This process is the owner and is obviously alive.
	if err := restoreIfGoneWith(h, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if !h.enf.locked() {
		t.Error("recovery unlocked a machine whose client is still running")
	}

	// A PID that is not running stands in for the client having been killed.
	if err := restoreIfGoneWith(h, deadPID(t)); err != nil {
		t.Fatal(err)
	}
	if h.enf.locked() {
		t.Errorf("recovery left the machine locked after its client vanished: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
}

// restoreIfGoneWith runs the task's decision against the harness's fake
// enforcer rather than the real machine.
func restoreIfGoneWith(h *guardHarness, pid int) error {
	g := newGuard(guardDeps{enf: h.enf, jrnlPath: h.path, logf: h.g.d.logf})
	j, err := g.readJournal()
	if err != nil || j.empty() {
		return err
	}
	if pid > 0 && processAlive(pid) {
		return nil
	}
	g.applied = j
	return g.Release("管控进程已消失")
}

// deadPID returns a process id that is certainly not running.
func deadPID(t *testing.T) int {
	t.Helper()
	for pid := 400000; pid < 400100; pid++ {
		if !processAlive(pid) {
			return pid
		}
	}
	t.Skip("找不到一个确定不存在的 pid")
	return 0
}

// Many labs revert their machines to a clean image on every restart. There the
// startup task cannot do its job - the restore wipes the task registration
// along with the lock it would have undone - so it is skipped. The
// every-minute task is unaffected, because a process-tree kill has nothing to
// do with rebooting.
func TestBootTaskSkippedWhereTheRoomRestoresItself(t *testing.T) {
	h := newGuardHarness(t)
	p := bothBlocked()
	p.RebootRestoresSystem = true

	if err := h.g.Apply(p, someInfo()); err != nil {
		t.Fatal(err)
	}
	if h.enf.tasks[recoveryTaskName] {
		t.Error("开机任务仍被安装；在会还原的机房里它无法生效")
	}
	if !h.enf.tasks[recoveryTickName] {
		t.Error("每分钟恢复任务被一起关掉了；它和重启无关，必须保留")
	}
	// The lock itself is unchanged - only the one recovery layer that cannot
	// work here is dropped.
	if h.enf.usbStart != 4 || len(h.enf.rules) != 2 {
		t.Errorf("管控本身受影响了: usb=%d rules=%v", h.enf.usbStart, h.enf.rules)
	}

	if err := h.g.Release("考试结束"); err != nil {
		t.Fatal(err)
	}
	if h.enf.locked() {
		t.Errorf("解除后仍有残留: usb=%d rules=%v tasks=%v",
			h.enf.usbStart, h.enf.rules, h.enf.tasks)
	}
}

// Switching the setting mid-exam must not leave the task from the previous
// setting running.
func TestChangingTheRoomSettingClearsAStaleBootTask(t *testing.T) {
	h := newGuardHarness(t)
	if err := h.g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatal(err)
	}
	if !h.enf.tasks[recoveryTaskName] {
		t.Fatal("前置条件：默认应安装开机任务")
	}

	p := bothBlocked()
	p.RebootRestoresSystem = true
	if err := h.g.Apply(p, someInfo()); err != nil {
		t.Fatal(err)
	}
	if h.enf.tasks[recoveryTaskName] {
		t.Error("改设置后旧的开机任务仍在")
	}
	if !h.enf.tasks[recoveryTickName] {
		t.Error("每分钟恢复任务丢了")
	}
}
