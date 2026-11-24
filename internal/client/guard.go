package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"coderoom/internal/protocol"
)

// Enforcement (M4): actually disabling USB storage and internet access on the
// student's machine, rather than only reporting on them.
//
// This is the most dangerous code in the project, and the danger is not that a
// student gets away with something - it is that a machine is left with no USB
// and no network after the exam, in a room where the teacher has no way to put
// it back. Fifty bricked machines is an incident; one undetected phone lookup
// is not. Every decision below follows from that asymmetry:
//
//   - Every failure path fails OPEN. Crash, kill, power cut, unreachable
//     server, expired exam: all of them end with the machine restored, never
//     with it left locked.
//   - Nothing is changed before the journal that reverses it is on disk and
//     flushed, and before the mechanism that will replay that journal without
//     this process exists.
//   - Startup restores unconditionally, then re-applies if the exam is still
//     running. Deciding whether a leftover lock is "still legitimate" would
//     need the network, which is exactly what may be missing.
//
// Five independent layers undo a lock, so that no single failure strands a
// machine:
//
//  1. normal release - exam ends, student logs out, policy changes, client exits
//  2. watchdog - a child process that restores when its parent dies
//  3. boot recovery - a scheduled task that restores at startup, as SYSTEM
//  4. teacher override - the server's emergency release command
//  5. the documented manual escape: coderoom-client.exe -restore

// journalVersion guards against a future format being read by an old binary
// that would then "restore" the wrong things.
const journalVersion = 1

// recoveryTaskName is the scheduled task that puts a machine back after a
// power cut. Its name is fixed so that any build can find and remove it.
const recoveryTaskName = "CodeRoomRecovery"

// usbSnapshot is the USBSTOR service's start type before CodeRoom touched it.
// 3 is the Windows default (manual start); 4 is disabled.
type usbSnapshot struct {
	Start uint32 `json:"start"`
}

// firewallSnapshot is the list of firewall rules CodeRoom adds, recorded in
// the journal *before* the first of them exists so that a crash half way
// through still leaves a complete list to delete.
//
// Rules rather than the profile's default outbound action, deliberately.
// Flipping the default to block means every restore has to put back a
// multi-valued setting faithfully (inbound action, notification suppression,
// per-profile), and getting that wrong silently weakens a machine's firewall
// for good. Deleting two rules by name cannot go half right, needs no parsing
// of localised netsh output, and leaves the machine's own configuration
// untouched the whole time.
type firewallSnapshot struct {
	Rules []string `json:"rules"`
}

// journal is the record that lets a process with no other context put a
// machine back exactly as it was. It is written before the first change and
// deleted after the last reversal.
type journal struct {
	Version   int    `json:"version"`
	AppliedAt int64  `json:"applied_at"`
	ExamID    int64  `json:"exam_id"`
	ExamName  string `json:"exam_name"`
	StudentNo string `json:"student_no"`

	// Original values, captured immediately before the corresponding change.
	// A nil field means that control was never touched and must not be
	// "restored" to a guess.
	USB      *usbSnapshot      `json:"usb,omitempty"`
	Firewall *firewallSnapshot `json:"firewall,omitempty"`
	Task     string            `json:"recovery_task,omitempty"`
}

func (j *journal) empty() bool {
	return j == nil || (j.USB == nil && j.Firewall == nil && j.Task == "")
}

// enforcer is the privileged half. Everything above it is portable and tested
// with a fake; everything below it edits the registry and the firewall.
type enforcer interface {
	// available reports whether this build and this process can enforce at
	// all, with a reason fit to show a teacher when it cannot. A client that
	// cannot enforce must say so loudly rather than appear to be locking a
	// machine it has no power over.
	available() (bool, string)

	snapshotUSB() (usbSnapshot, error)
	disableUSBStorage() error
	restoreUSB(usbSnapshot) error

	snapshotFirewall() (firewallSnapshot, error)
	blockInternet(allowHosts []string) error
	restoreFirewall(firewallSnapshot) error

	// installRecovery registers something that will restore this machine at
	// next boot even if no CodeRoom process ever runs again.
	installRecovery(taskName string) error
	removeRecovery(taskName string) error
}

// guardDeps is what the guard needs from its surroundings.
type guardDeps struct {
	enf      enforcer
	jrnlPath string
	emit     func(protocol.Event)
	logf     func(string, ...any)
	now      func() time.Time
}

// Guard owns the lifecycle of an applied policy.
type Guard struct {
	d  guardDeps
	mu sync.Mutex

	applied     *journal // non-nil while this process holds a lock
	lastErr     string
	lastEmitted string // suppresses repeating a standing condition
}

func newGuard(d guardDeps) *Guard {
	if d.now == nil {
		d.now = time.Now
	}
	if d.logf == nil {
		d.logf = func(string, ...any) {}
	}
	if d.emit == nil {
		d.emit = func(protocol.Event) {}
	}
	return &Guard{d: d}
}

// GuardStatus is the student- and teacher-visible state of enforcement.
type GuardStatus struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	Active    bool   `json:"active"`
	USB       bool   `json:"usb_blocked"`
	Internet  bool   `json:"internet_blocked"`
	LastError string `json:"last_error"`
}

func (g *Guard) Status() GuardStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	ok, reason := g.d.enf.available()
	st := GuardStatus{Supported: ok, Reason: reason, LastError: g.lastErr}
	if g.applied != nil {
		st.Active = true
		st.USB = g.applied.USB != nil
		st.Internet = g.applied.Firewall != nil
	}
	return st
}

// ------------------------------------------------------------------ journal

func (g *Guard) readJournal() (*journal, error) {
	raw, err := os.ReadFile(g.d.jrnlPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		// A corrupt journal is the worst case: something was changed and the
		// record of what is unreadable. Say so rather than deleting it, so a
		// teacher reading the log knows to check the machine by hand.
		return nil, fmt.Errorf("恢复记录已损坏 (%s): %w", g.d.jrnlPath, err)
	}
	if j.Version != journalVersion {
		return nil, fmt.Errorf("恢复记录版本 %d 无法识别（本程序支持 %d）",
			j.Version, journalVersion)
	}
	return &j, nil
}

// writeJournal persists the record and flushes it, because the whole point is
// surviving a power cut. A write that is still in the page cache when the
// lights go out is the same as no journal at all.
func (g *Guard) writeJournal(j *journal) error {
	if err := os.MkdirAll(filepath.Dir(g.d.jrnlPath), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.d.jrnlPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, g.d.jrnlPath)
}

// ------------------------------------------------------------------- apply

// applyInfo identifies the session a lock belongs to, for the journal and for
// the teacher's event log.
type applyInfo struct {
	ExamID    int64
	ExamName  string
	StudentNo string
}

// Apply puts the policy's enforcement into effect. It is safe to call on every
// heartbeat: if the same controls are already held by this process it does
// nothing.
func (g *Guard) Apply(p protocol.Policy, info applyInfo) error {
	wantUSB, wantNet := p.BlockUSBStorage, p.BlockInternet
	if !wantUSB && !wantNet {
		return g.Release("策略不再要求管控")
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if g.holds(wantUSB, wantNet) {
		return nil
	}
	ok, reason := g.d.enf.available()
	if !ok {
		g.lastErr = reason
		g.emitOnce(protocol.EvGuardUnavailable, protocol.SevAlert, map[string]any{
			"reason": reason, "want_usb": wantUSB, "want_internet": wantNet,
		})
		return errors.New(reason)
	}

	// Anything already held is released first, so that a policy change never
	// leaves a half-applied machine whose journal describes a different shape.
	if g.applied != nil {
		if err := g.releaseLocked("策略变更，先恢复再重新应用"); err != nil {
			return err
		}
	}

	j := &journal{
		Version:   journalVersion,
		AppliedAt: g.d.now().UnixMilli(),
		ExamID:    info.ExamID,
		ExamName:  info.ExamName,
		StudentNo: info.StudentNo,
		Task:      recoveryTaskName,
	}

	// Capture everything that is about to change, before changing any of it.
	if wantUSB {
		snap, err := g.d.enf.snapshotUSB()
		if err != nil {
			return g.failed("读取 U 盘策略当前状态失败", err)
		}
		j.USB = &snap
	}
	if wantNet {
		snap, err := g.d.enf.snapshotFirewall()
		if err != nil {
			return g.failed("读取防火墙当前状态失败", err)
		}
		j.Firewall = &snap
	}

	// The journal and the boot-time recovery both have to exist before the
	// first change. Reversed, a power cut in between would leave a locked
	// machine with nothing on it that knows how to unlock.
	if err := g.writeJournal(j); err != nil {
		return g.failed("写入恢复记录失败", err)
	}
	if err := g.d.enf.installRecovery(recoveryTaskName); err != nil {
		_ = os.Remove(g.d.jrnlPath)
		return g.failed("安装开机恢复任务失败", err)
	}

	// Only now is anything actually changed. A failure here rolls back what
	// has been done so far rather than leaving the machine in between.
	if wantUSB {
		if err := g.d.enf.disableUSBStorage(); err != nil {
			g.applied = j
			_ = g.releaseLocked("应用失败，回滚")
			return g.failed("禁用 U 盘存储失败", err)
		}
	}
	if wantNet {
		if err := g.d.enf.blockInternet(p.AllowedHosts); err != nil {
			g.applied = j
			_ = g.releaseLocked("应用失败，回滚")
			return g.failed("禁用外网访问失败", err)
		}
	}

	g.applied = j
	g.lastErr = ""
	g.d.logf("已应用考场管控：U盘=%v 外网=%v（恢复记录 %s）", wantUSB, wantNet, g.d.jrnlPath)
	g.d.emit(protocol.Event{
		Type:       protocol.EvGuardApplied,
		Severity:   protocol.SevInfo,
		ClientTime: g.d.now().UnixMilli(),
		Payload: map[string]any{
			"usb_blocked": wantUSB, "internet_blocked": wantNet,
		},
	})
	return nil
}

func (g *Guard) holds(usb, net bool) bool {
	if g.applied == nil {
		return false
	}
	return (g.applied.USB != nil) == usb && (g.applied.Firewall != nil) == net
}

func (g *Guard) failed(what string, err error) error {
	g.lastErr = what + ": " + err.Error()
	g.d.logf("考场管控失败：%s", g.lastErr)
	g.d.emit(protocol.Event{
		Type:       protocol.EvGuardFailed,
		Severity:   protocol.SevAlert,
		ClientTime: g.d.now().UnixMilli(),
		Payload:    map[string]any{"stage": what, "error": err.Error()},
	})
	return fmt.Errorf("%s: %w", what, err)
}

// emitOnce avoids repeating a standing condition (such as "not running as
// administrator") on every heartbeat.
func (g *Guard) emitOnce(typ, sev string, payload map[string]any) {
	if g.lastEmitted == typ {
		return
	}
	g.lastEmitted = typ
	g.d.emit(protocol.Event{
		Type: typ, Severity: sev,
		ClientTime: g.d.now().UnixMilli(), Payload: payload,
	})
}

// ----------------------------------------------------------------- release

// Release undoes whatever is recorded in the journal. It is idempotent and
// safe to call when nothing is applied, which is what lets every exit path
// call it without first working out whether it needs to.
func (g *Guard) Release(reason string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.releaseLocked(reason)
}

func (g *Guard) releaseLocked(reason string) error {
	j := g.applied
	if j == nil {
		// Nothing held by this process - but something may still be recorded
		// on disk from a run that died. That is SelfCheck's job, and doing it
		// here too costs one stat call and closes the window where a caller
		// releases before ever having applied.
		disk, err := g.readJournal()
		if err != nil {
			g.lastErr = err.Error()
			return err
		}
		if disk.empty() {
			return nil
		}
		j = disk
	}

	var firstErr error
	fail := func(what string, err error) {
		if err == nil {
			return
		}
		g.d.logf("恢复 %s 失败：%v", what, err)
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", what, err)
		}
	}

	// Reversed order, and every step is attempted even if an earlier one
	// failed: a machine with its network back but its USB still disabled is
	// better than one that stopped at the first error.
	if j.Firewall != nil {
		fail("防火墙", g.d.enf.restoreFirewall(*j.Firewall))
	}
	if j.USB != nil {
		fail("U 盘策略", g.d.enf.restoreUSB(*j.USB))
	}
	if j.Task != "" {
		fail("开机恢复任务", g.d.enf.removeRecovery(j.Task))
	}

	if firstErr != nil {
		// The journal stays on disk so the next attempt - watchdog, next
		// launch, or the boot task - tries again.
		g.lastErr = firstErr.Error()
		g.d.emit(protocol.Event{
			Type:       protocol.EvGuardFailed,
			Severity:   protocol.SevAlert,
			ClientTime: g.d.now().UnixMilli(),
			Payload:    map[string]any{"stage": "恢复", "error": firstErr.Error()},
		})
		return firstErr
	}

	if err := os.Remove(g.d.jrnlPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		fail("删除恢复记录", err)
		return firstErr
	}
	g.applied = nil
	g.lastErr = ""
	g.lastEmitted = ""
	g.d.logf("已解除考场管控：%s", reason)
	g.d.emit(protocol.Event{
		Type:       protocol.EvGuardReleased,
		Severity:   protocol.SevInfo,
		ClientTime: g.d.now().UnixMilli(),
		Payload:    map[string]any{"reason": reason},
	})
	return nil
}

// --------------------------------------------------------------- self-check

// SelfCheck runs before anything else at startup and undoes any lock left
// behind by a previous run.
//
// It restores unconditionally rather than trying to decide whether the lock is
// still legitimate. Making that judgement needs the server, and the server is
// exactly what may be unreachable - a machine must never stay locked because
// it could not ask permission to unlock. If the exam really is still running,
// the client re-applies within one heartbeat of logging back in.
func (g *Guard) SelfCheck() {
	g.mu.Lock()
	defer g.mu.Unlock()

	j, err := g.readJournal()
	if err != nil {
		g.lastErr = err.Error()
		g.d.logf("自检：%v —— 请手动检查本机 U 盘与网络设置", err)
		return
	}
	if j.empty() {
		return
	}

	left := time.Duration(g.d.now().UnixMilli()-j.AppliedAt) * time.Millisecond
	g.d.logf("自检：发现上次运行遗留的管控（考试「%s」，%s 前应用），正在恢复",
		j.ExamName, left.Round(time.Second))

	g.applied = j
	if err := g.releaseLocked("启动自检发现遗留管控"); err != nil {
		g.d.logf("自检恢复失败：%v", err)
		return
	}
	// Worth telling the teacher: a machine that needed this was left locked
	// after a crash or a power cut, and somebody should know it happened.
	g.d.emit(protocol.Event{
		Type:       protocol.EvGuardRecovered,
		Severity:   protocol.SevWarn,
		ClientTime: g.d.now().UnixMilli(),
		Payload: map[string]any{
			"exam_id": j.ExamID, "exam_name": j.ExamName,
			"applied_at": j.AppliedAt, "locked_for_ms": left.Milliseconds(),
		},
	})
}

// RestoreOnly is the entry point for `-restore` and for the boot-time
// recovery task: put the machine back, report what happened, and exit. It
// deliberately needs no server, no login and no configuration.
func RestoreOnly(dataDir string, logf func(string, ...any)) error {
	g := newGuard(guardDeps{
		enf:      newEnforcer(),
		jrnlPath: guardJournalPath(dataDir),
		logf:     logf,
	})
	j, err := g.readJournal()
	if err != nil {
		return err
	}
	if j.empty() {
		logf("本机没有待恢复的考场管控。")
		return nil
	}
	logf("发现考试「%s」遗留的管控，正在恢复…", j.ExamName)
	g.applied = j
	if err := g.Release("手动恢复"); err != nil {
		return err
	}
	logf("已恢复：U 盘与网络设置均已还原。")
	return nil
}

// ---------------------------------------------------------------- watchdog

// WatchParent is the second recovery layer: a child process that does nothing
// but wait for its parent to die and then put the machine back.
//
// It exists because the student can kill the client - any process running as
// them - and the boot-time task would otherwise leave the machine locked until
// the next restart. Killing the client is itself reported to the teacher as a
// lost heartbeat, so failing open here costs nothing that is not already seen.
func WatchParent(ctx context.Context, parentPID int, dataDir string, logf func(string, ...any)) {
	logf("看门狗已启动，监视主进程 %d", parentPID)
	waitForProcessExit(ctx, parentPID)
	if ctx.Err() != nil {
		return
	}
	logf("主进程 %d 已退出，执行恢复", parentPID)
	if err := RestoreOnly(dataDir, logf); err != nil {
		logf("看门狗恢复失败：%v —— 开机恢复任务会在下次启动时再试", err)
	}
}
