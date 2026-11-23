//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Windows Firewall blocks inbound connections to a new executable by default,
// which means a freshly unzipped server listens happily while every student
// machine times out. Discovered on real hardware: the listener was up, the
// banner advertised the LAN address, and nothing could reach it.
//
// The rules are scoped to the *program* rather than to a port. A port-scoped
// rule stops applying the moment a teacher changes the port, and the only
// symptom is "students cannot connect" - the same failure this is meant to
// prevent. Also learned on real hardware: a rule added for the default port
// left a run on another port just as unreachable.

const (
	ruleTCP = "CodeRoom 判题服务端 (TCP)"
	ruleUDP = "CodeRoom 自动发现 (UDP)"
)

// legacyRuleNames are the port-scoped rules earlier builds created. They are
// removed during setup so an upgraded install does not accumulate stale
// entries pointing at ports that are no longer in use.
func legacyRuleNames(tcpPort, udpPort int) []string {
	return []string{
		fmt.Sprintf("CodeRoom TCP %d", tcpPort),
		fmt.Sprintf("CodeRoom UDP %d", udpPort),
	}
}

// netsh builds a hidden netsh invocation, so no console flashes.
func netsh(args ...string) *exec.Cmd {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func serverExePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := os.Readlink(exe); err == nil && resolved != "" {
		exe = resolved
	}
	return exe, nil
}

// FirewallReady reports whether both rules exist and still point at this
// executable. Checking the path catches the case where the folder was moved
// after the rules were added.
//
// It looks for the executable path in netsh's output rather than parsing field
// names, because those are localised and the path is not.
func FirewallReady(tcpPort, udpPort int) bool {
	exe, err := serverExePath()
	if err != nil {
		return false
	}
	for _, name := range []string{ruleTCP, ruleUDP} {
		out, err := netsh("advfirewall", "firewall", "show", "rule",
			"name="+name, "verbose").CombinedOutput()
		if err != nil {
			return false
		}
		if !strings.Contains(strings.ToLower(string(out)), strings.ToLower(exe)) {
			return false
		}
	}
	return true
}

// SetupFirewall adds inbound allow rules for this executable. Requires
// elevation; without it netsh refuses and the error says so.
func SetupFirewall(tcpPort, udpPort int) error {
	exe, err := serverExePath()
	if err != nil {
		return fmt.Errorf("无法确定本程序路径: %w", err)
	}

	// Clear anything we may have added before, under either naming scheme, so
	// re-running is idempotent and leaves no stale rules.
	for _, name := range append(legacyRuleNames(tcpPort, udpPort), ruleTCP, ruleUDP) {
		_ = netsh("advfirewall", "firewall", "delete", "rule", "name="+name).Run()
	}

	for _, r := range []struct{ name, proto, desc string }{
		{ruleTCP, "TCP", "学生端连接教师端使用的 HTTP 端口"},
		{ruleUDP, "UDP", "学生端自动发现教师端使用的广播端口"},
	} {
		out, err := netsh("advfirewall", "firewall", "add", "rule",
			"name="+r.name,
			"description="+r.desc,
			"dir=in", "action=allow",
			"program="+exe,
			"protocol="+r.proto,
			"profile=any",
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("添加防火墙规则「%s」失败: %w\n%s\n"+
				"（这一步需要管理员权限：右键 coderoom-server.exe → 以管理员身份运行）",
				r.name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// FirewallHint is the one-liner shown when the rules are missing or stale.
func FirewallHint(tcpPort, udpPort int) string {
	return "学生端可能连不上：Windows 防火墙默认拦截入站连接。\n" +
		"     以管理员身份运行一次即可放行（换端口或移动目录后需重新运行）：\n" +
		"       coderoom-server.exe -setup-firewall"
}
