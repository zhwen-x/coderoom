//go:build windows

package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"coderoom/internal/protocol"
)

// Enforcement against the real registry and the real firewall.
//
// These tests change the machine they run on, so they are off unless
// CODEROOM_ENFORCE_TEST=1 is set. A casual `go test ./...` must never disable
// somebody's USB ports - the whole milestone is about machines not being left
// in a state nobody asked for, and that has to start here.
//
//	CODEROOM_ENFORCE_TEST=1 go test ./internal/client/ -run TestReal -v
//
// Every test registers a cleanup that puts the machine back using the values
// read at the start, through the enforcer primitives directly rather than
// through the Guard - the Guard is what is under test and must not also be
// the only thing that can undo the test.

func requireEnforceable(t *testing.T) *winEnforcer {
	t.Helper()
	if os.Getenv("CODEROOM_ENFORCE_TEST") != "1" {
		t.Skip("会真实修改本机设置；需要 CODEROOM_ENFORCE_TEST=1 才运行")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("需要管理员权限")
	}
	enf := &winEnforcer{}
	if ok, reason := enf.available(); !ok {
		t.Skipf("本机无法执行管控: %s", reason)
	}
	return enf
}

// guardRail restores the machine from values read before anything happened,
// whatever the test does or how it fails.
func guardRail(t *testing.T, enf *winEnforcer) usbSnapshot {
	t.Helper()
	original, err := enf.snapshotUSB()
	if err != nil {
		t.Fatalf("读取 USBSTOR 原值失败: %v", err)
	}
	t.Logf("本机原始 USBSTOR Start = %d", original.Start)

	t.Cleanup(func() {
		if err := enf.restoreUSB(original); err != nil {
			t.Errorf("清理失败：USBSTOR 未能还原为 %d: %v", original.Start, err)
		}
		_ = deleteFirewallRule(fwRuleV4)
		_ = deleteFirewallRule(fwRuleV6)
		_, _ = schtasksOutput("/Delete", "/TN", recoveryTaskName, "/F")
		_, _ = schtasksOutput("/Delete", "/TN", recoveryTickName, "/F")
		_ = os.Remove(guardJournalPath(t.TempDir()))
		if pd := os.Getenv("ProgramData"); pd != "" {
			_ = os.Remove(filepath.Join(pd, "CodeRoom", "guard.json"))
		}
		if got, err := enf.snapshotUSB(); err == nil && got.Start != original.Start {
			t.Errorf("清理后 USBSTOR 仍为 %d，应为 %d", got.Start, original.Start)
		}
	})
	return original
}

func usbStartNow(t *testing.T) uint32 {
	t.Helper()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, usbstorKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatalf("打开 USBSTOR: %v", err)
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(usbstorValue)
	if err != nil {
		t.Fatalf("读取 Start: %v", err)
	}
	return uint32(v)
}

func firewallRuleExists(name string) bool {
	return exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+name).Run() == nil
}

func taskExists(name string) bool {
	return exec.Command("schtasks", "/Query", "/TN", name).Run() == nil
}

// bothTasksExist: the boot trigger and the every-minute trigger are separate
// tasks, and a machine is only properly covered when both are present.
func bothTasksExist() bool {
	return taskExists(recoveryTaskName) && taskExists(recoveryTickName)
}

func newRealGuard(t *testing.T, enf *winEnforcer) (*Guard, string, *[]protocol.Event) {
	t.Helper()
	// The journal goes to a temp path rather than ProgramData so a test never
	// competes with a real client running on the same machine.
	path := filepath.Join(t.TempDir(), "guard.json")
	events := &[]protocol.Event{}
	g := newGuard(guardDeps{
		enf:      enf,
		jrnlPath: path,
		emit:     func(ev protocol.Event) { *events = append(*events, ev) },
		logf:     t.Logf,
	})
	return g, path, events
}

// The whole milestone in one test: lock the machine for real, confirm every
// control actually took effect, then confirm the machine is exactly as it was.
func TestRealApplyAndRelease(t *testing.T) {
	enf := requireEnforceable(t)
	original := guardRail(t, enf)
	g, path, _ := newRealGuard(t, enf)

	if err := g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatalf("应用管控失败: %v", err)
	}

	if got := usbStartNow(t); got != usbStartDisabled {
		t.Errorf("USBSTOR Start = %d，应为 %d（U 盘并未真的被禁用）", got, usbStartDisabled)
	}
	for _, name := range []string{fwRuleV4, fwRuleV6} {
		if !firewallRuleExists(name) {
			t.Errorf("防火墙规则 %s 不存在（外网并未真的被封锁）", name)
		}
	}
	if !bothTasksExist() {
		t.Errorf("恢复任务不完整：开机=%v 每分钟=%v",
			taskExists(recoveryTaskName), taskExists(recoveryTickName))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("恢复账本未落盘: %v", err)
	}

	if err := g.Release("测试结束"); err != nil {
		t.Fatalf("解除管控失败: %v", err)
	}

	if got := usbStartNow(t); got != original.Start {
		t.Errorf("USBSTOR Start = %d，应还原为 %d", got, original.Start)
	}
	for _, name := range []string{fwRuleV4, fwRuleV6} {
		if firewallRuleExists(name) {
			t.Errorf("防火墙规则 %s 残留", name)
		}
	}
	if taskExists(recoveryTaskName) || taskExists(recoveryTickName) {
		t.Errorf("恢复任务残留：开机=%v 每分钟=%v",
			taskExists(recoveryTaskName), taskExists(recoveryTickName))
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("恢复账本残留")
	}
}

// The crash case against real state: a journal and a locked machine, no
// process that remembers applying it. Startup has to put it back.
func TestRealSelfCheckRecoversALockedMachine(t *testing.T) {
	enf := requireEnforceable(t)
	original := guardRail(t, enf)
	g, path, _ := newRealGuard(t, enf)

	if err := g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatalf("应用管控失败: %v", err)
	}
	if usbStartNow(t) != usbStartDisabled {
		t.Fatal("前置条件：U 盘应已被禁用")
	}

	// A new Guard over the same journal is what the next launch sees.
	fresh := newGuard(guardDeps{enf: enf, jrnlPath: path, logf: t.Logf})
	fresh.SelfCheck()

	if got := usbStartNow(t); got != original.Start {
		t.Errorf("自检后 USBSTOR Start = %d，应还原为 %d", got, original.Start)
	}
	if firewallRuleExists(fwRuleV4) || firewallRuleExists(fwRuleV6) {
		t.Error("自检后防火墙规则仍在")
	}
	if taskExists(recoveryTaskName) || taskExists(recoveryTickName) {
		t.Error("自检后恢复任务仍在")
	}
}

// RestoreOnly is the documented manual escape and the command the boot task
// runs. It has to work from nothing but the journal on disk.
func TestRealRestoreOnlyFromJournalAlone(t *testing.T) {
	enf := requireEnforceable(t)
	original := guardRail(t, enf)

	// Apply through a guard whose journal lives where RestoreOnly will look.
	dataDir := t.TempDir()
	g := newGuard(guardDeps{enf: enf, jrnlPath: guardJournalPath(dataDir), logf: t.Logf})
	if err := g.Apply(bothBlocked(), someInfo()); err != nil {
		t.Fatalf("应用管控失败: %v", err)
	}
	if usbStartNow(t) != usbStartDisabled {
		t.Fatal("前置条件：U 盘应已被禁用")
	}

	if err := RestoreOnly(dataDir, t.Logf); err != nil {
		t.Fatalf("-restore 失败: %v", err)
	}
	if got := usbStartNow(t); got != original.Start {
		t.Errorf("-restore 后 USBSTOR Start = %d，应还原为 %d", got, original.Start)
	}
	if taskExists(recoveryTaskName) || taskExists(recoveryTickName) {
		t.Error("-restore 后恢复任务仍在")
	}

	// And running it again with nothing left is a quiet no-op, because that is
	// what a teacher will do when unsure whether a machine needs it.
	if err := RestoreOnly(dataDir, t.Logf); err != nil {
		t.Errorf("对已恢复的机器再次运行 -restore 报错: %v", err)
	}
}

// The boot task has to carry a command line that actually runs. A task that
// exists but whose action is wrong looks like protection and provides none.
func TestRealRecoveryTaskPointsAtThisExecutable(t *testing.T) {
	enf := requireEnforceable(t)
	guardRail(t, enf)

	if err := enf.installRecovery(recoveryTaskName, os.Getpid()); err != nil {
		t.Fatalf("安装开机恢复任务失败: %v", err)
	}
	out, err := exec.Command("schtasks", "/Query", "/TN", recoveryTaskName, "/FO", "LIST", "/V").Output()
	if err != nil {
		t.Fatalf("查询任务失败: %v", err)
	}
	text := string(out)
	exe, _ := os.Executable()
	if !containsFold(text, filepath.Base(exe)) {
		t.Errorf("任务命令行里没有本程序 %s:\n%s", filepath.Base(exe), text)
	}
	if !containsFold(text, "-restore-if-gone") {
		t.Errorf("任务没有带 -restore-if-gone 参数:\n%s", text)
	}
	// The repetition is what covers a process-tree kill, which takes the
	// child watchdog with it. Without it the machine stays locked until the
	// next boot.
	if !containsFold(text, "1 分钟") && !containsFold(text, "Minute") &&
		!containsFold(text, "0:01:00") {
		t.Logf("任务重复间隔未在输出中识别出来，请人工确认：\n%s", text)
	}
	if err := enf.removeRecovery(recoveryTaskName); err != nil {
		t.Errorf("删除任务失败: %v", err)
	}
	if taskExists(recoveryTaskName) || taskExists(recoveryTickName) {
		t.Error("任务删除后仍存在")
	}
}

func containsFold(haystack, needle string) bool {
	h, n := []rune(haystack), []rune(needle)
	if len(n) == 0 {
		return true
	}
	lower := func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return r
	}
	for i := 0; i+len(n) <= len(h); i++ {
		ok := true
		for j := range n {
			if lower(h[i+j]) != lower(n[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
