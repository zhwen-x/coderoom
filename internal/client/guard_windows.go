//go:build windows

package client

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The privileged half of enforcement on Windows. Three things are changed and
// each is reversible by a single, exact operation:
//
//   - USB storage: one registry DWORD, restored to the value read before.
//   - Internet: two firewall rules with fixed names, deleted by name.
//   - Boot recovery: one scheduled task with a fixed name, deleted by name.
//
// Nothing here parses localised command output to decide what to do. That
// lesson came from M2, where a firewall check that read netsh's human-readable
// text would have behaved differently on a Chinese-language machine than on an
// English one.

const (
	usbstorKey       = `SYSTEM\CurrentControlSet\Services\USBSTOR`
	usbstorValue     = "Start"
	usbStartManual   = 3 // Windows default: driver loads when a device appears
	usbStartDisabled = 4

	fwRuleV4 = "CodeRoom-BlockInternet-v4"
	fwRuleV6 = "CodeRoom-BlockInternet-v6"
)

// publicIPv4Ranges is every IPv4 address that is not on a local network.
//
// Blocking the complement of the private ranges, rather than blocking
// everything and allowing exceptions back, is forced by how Windows Firewall
// resolves conflicts: a block rule beats an allow rule, so "block all, allow
// the server" does not work. It is also what the requirement actually wants -
// students must not reach the internet, while the judge server, the classroom
// software and any file share on the LAN have to keep working.
//
// Excluded here: 0.0.0.0/8, 10/8, 127/8, 169.254/16, 172.16/12, 192.168/16,
// and everything from 224.0.0.0 up (multicast and reserved).
var publicIPv4Ranges = strings.Join([]string{
	"1.0.0.0-9.255.255.255",
	"11.0.0.0-126.255.255.255",
	"128.0.0.0-169.253.255.255",
	"169.255.0.0-172.15.255.255",
	"172.32.0.0-192.167.255.255",
	"192.169.0.0-223.255.255.255",
}, ",")

// publicIPv6Range is global unicast. Unique-local (fc00::/7) and link-local
// (fe80::/10) sit outside it and stay reachable, matching the IPv4 rule.
//
// A lab with IPv6 internet and only the IPv4 rule in place would look locked
// while a browser quietly preferred IPv6 - a silent hole, which is the worst
// kind.
const publicIPv6Range = "2000::/3"

func newEnforcer() enforcer { return &winEnforcer{} }

type winEnforcer struct{}

// available reports whether this process can actually change anything.
//
// Being honest here matters more than it looks. A client that cannot elevate
// but reports success would leave a teacher believing a room is locked down
// when it is not - and they would find out only afterwards, from the results.
func (w *winEnforcer) available() (bool, string) {
	if !isElevated() {
		return false, "客户端没有管理员权限，无法执行禁用 U 盘 / 断网等管控操作。" +
			"请以管理员身份运行学生端，或在机房镜像中将其配置为管理员启动。"
	}
	// Prove it rather than assume it: an elevated token still fails if group
	// policy has locked the key, and finding that out at apply time means
	// finding it out after the journal has been written.
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, usbstorKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		return false, fmt.Sprintf("无法写入 U 盘策略注册表项（%v），"+
			"可能被组策略或安全软件锁定。", err)
	}
	k.Close()
	return true, ""
}

func isElevated() bool {
	return windows.GetCurrentProcessToken().IsElevated()
}

// --------------------------------------------------------------- USB storage

func (w *winEnforcer) snapshotUSB() (usbSnapshot, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, usbstorKey, registry.QUERY_VALUE)
	if err != nil {
		return usbSnapshot{}, fmt.Errorf("打开 %s: %w", usbstorKey, err)
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(usbstorValue)
	if err != nil {
		return usbSnapshot{}, fmt.Errorf("读取 %s\\%s: %w", usbstorKey, usbstorValue, err)
	}
	return usbSnapshot{Start: uint32(v)}, nil
}

// disableUSBStorage stops the USB mass-storage driver from loading for newly
// attached devices.
//
// It does not eject a stick that is already plugged in: the driver is already
// loaded for it and stays loaded until it is removed. That gap is real and is
// documented for teachers rather than papered over - the M3 monitor still
// reports a drive that was already there, so the teacher sees it either way.
func (w *winEnforcer) disableUSBStorage() error {
	return setUSBStart(usbStartDisabled)
}

func (w *winEnforcer) restoreUSB(s usbSnapshot) error {
	// Restore exactly what was read, not a hard-coded 3: a machine whose
	// administrator had already disabled USB storage must stay that way.
	start := s.Start
	if start == 0 {
		// Never observed in practice (0 is "boot start", meaningless for
		// USBSTOR), but a zero here almost certainly means a journal written
		// before the value was read. Manual is the Windows default and the
		// safe side to err on: a working USB port, not a dead one.
		start = usbStartManual
	}
	return setUSBStart(start)
}

func setUSBStart(v uint32) error {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, usbstorKey, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 %s: %w", usbstorKey, err)
	}
	defer k.Close()
	if err := k.SetDWordValue(usbstorValue, v); err != nil {
		return fmt.Errorf("设置 %s\\%s=%d: %w", usbstorKey, usbstorValue, v, err)
	}
	return nil
}

// ----------------------------------------------------------------- firewall

func (w *winEnforcer) snapshotFirewall() (firewallSnapshot, error) {
	return firewallSnapshot{Rules: []string{fwRuleV4, fwRuleV6}}, nil
}

func (w *winEnforcer) blockInternet(allowHosts []string) error {
	// allowHosts is accepted and ignored: everything on the local network is
	// already permitted by construction, which covers the judge server and the
	// classroom software. Carving a public address back out of the blocked
	// ranges is the kind of change that fails open without anyone noticing, so
	// it waits until something actually needs it.
	_ = allowHosts

	// Delete first: a leftover rule from a previous run would otherwise make
	// `add` fail or produce a duplicate that restore only half removes.
	_ = deleteFirewallRule(fwRuleV4)
	_ = deleteFirewallRule(fwRuleV6)

	if err := addBlockRule(fwRuleV4, publicIPv4Ranges); err != nil {
		return err
	}
	if err := addBlockRule(fwRuleV6, publicIPv6Range); err != nil {
		_ = deleteFirewallRule(fwRuleV4)
		return err
	}
	return nil
}

func (w *winEnforcer) restoreFirewall(s firewallSnapshot) error {
	rules := s.Rules
	if len(rules) == 0 {
		// An older or truncated journal: fall back to the names this build
		// knows, so a machine is never left blocked for want of a list.
		rules = []string{fwRuleV4, fwRuleV6}
	}
	var firstErr error
	for _, name := range rules {
		if err := deleteFirewallRule(name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func addBlockRule(name, remoteIP string) error {
	return netsh("advfirewall", "firewall", "add", "rule",
		"name="+name, "dir=out", "action=block", "enable=yes",
		"profile=any", "remoteip="+remoteIP,
		"description=CodeRoom 考试期间临时禁止访问外网，考试结束自动删除")
}

// deleteFirewallRule removes a rule by exact name. Deleting a rule that is not
// there is reported by netsh as an error; that is a success for our purposes,
// since the goal is "this rule does not exist".
func deleteFirewallRule(name string) error {
	out, err := netshOutput("advfirewall", "firewall", "delete", "rule", "name="+name)
	if err == nil {
		return nil
	}
	if ruleAbsent(name) {
		return nil
	}
	return fmt.Errorf("删除防火墙规则 %s 失败: %w (%s)", name, err, strings.TrimSpace(out))
}

// ruleAbsent asks the firewall whether a rule exists, by exit status rather
// than by reading netsh's localised text.
func ruleAbsent(name string) bool {
	_, err := netshOutput("advfirewall", "firewall", "show", "rule", "name="+name)
	return err != nil
}

func netsh(args ...string) error {
	out, err := netshOutput(args...)
	if err != nil {
		return fmt.Errorf("netsh %s: %w (%s)",
			strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func netshOutput(args ...string) (string, error) {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ------------------------------------------------------------ boot recovery

// installRecovery registers a startup task that restores this machine even if
// no CodeRoom process ever runs on it again.
//
// This is the layer that covers the case nothing else can: the power goes out
// mid-exam. The journal survives on disk, but something has to read it, and
// the client may not be launched for days. A SYSTEM task at boot needs no
// student, no login and no network.
func (w *winEnforcer) installRecovery(taskName string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位本程序路径: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// schtasks takes the command as one argument; the inner quotes are what
	// survive a path with spaces, which "Program Files" guarantees.
	action := fmt.Sprintf(`"%s" -restore`, exe)
	return schtasks("/Create", "/TN", taskName, "/TR", action,
		"/SC", "ONSTART", "/RU", "SYSTEM", "/RL", "HIGHEST", "/F")
}

func (w *winEnforcer) removeRecovery(taskName string) error {
	out, err := schtasksOutput("/Delete", "/TN", taskName, "/F")
	if err == nil {
		return nil
	}
	if _, qErr := schtasksOutput("/Query", "/TN", taskName); qErr != nil {
		return nil // already gone, which is the state we wanted
	}
	return fmt.Errorf("删除计划任务 %s 失败: %w (%s)", taskName, err, strings.TrimSpace(out))
}

func schtasks(args ...string) error {
	out, err := schtasksOutput(args...)
	if err != nil {
		return fmt.Errorf("schtasks %s: %w (%s)",
			strings.Join(args, " "), err, strings.TrimSpace(out))
	}
	return nil
}

func schtasksOutput(args ...string) (string, error) {
	cmd := exec.Command("schtasks", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// -------------------------------------------------------------------- paths

// guardJournalPath puts the recovery record somewhere a student cannot delete.
//
// Not in the portable data directory: that sits next to the executable, in the
// student's own hands, and deleting it would strand the machine with no record
// of what to undo. ProgramData needs administrator rights to write - which the
// process has, since nothing is enforced without them.
func guardJournalPath(dataDir string) string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "CodeRoom", "guard.json")
	}
	return filepath.Join(dataDir, "guard.json")
}

// ----------------------------------------------------------------- watchdog

// waitForProcessExit blocks until the given process ends. The handle wait is
// exact: no polling interval during which a killed client leaves the machine
// locked.
func waitForProcessExit(ctx context.Context, pid int) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return // already gone, or not ours to wait on: treat as exited
	}
	defer windows.CloseHandle(h)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = windows.WaitForSingleObject(h, windows.INFINITE)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// spawnWatchdog starts this executable again as a watchdog for the current
// process.
func spawnWatchdog(dataDir string) (*os.Process, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "-watchdog", fmt.Sprint(os.Getpid()), "-data", dataDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// Its own process group, so a Ctrl+C aimed at the client does not also
		// kill the thing whose job is to clean up after the client.
		CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}
