//go:build windows

package server

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// Windows Firewall blocks inbound connections to a new executable by default,
// which means a freshly unzipped server listens happily while every student
// machine times out. Discovered on real hardware: the listener was up and the
// LAN could not reach it over either port.
//
// These helpers let the teacher fix that in one step instead of finding out
// five minutes before an exam.

func ruleName(proto string, port int) string {
	return fmt.Sprintf("CodeRoom %s %d", proto, port)
}

// hidden keeps netsh from flashing a console window.
func netsh(args ...string) *exec.Cmd {
	cmd := exec.Command("netsh", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// FirewallReady reports whether inbound rules for both ports already exist.
// A false result is advisory: the teacher may have allowed the ports by some
// other means, so this never blocks startup.
func FirewallReady(tcpPort, udpPort int) bool {
	for _, r := range []struct {
		proto string
		port  int
	}{{"TCP", tcpPort}, {"UDP", udpPort}} {
		out, err := netsh("advfirewall", "firewall", "show", "rule",
			"name="+ruleName(r.proto, r.port)).CombinedOutput()
		text := string(out)
		// netsh is localised, so check both wordings.
		enabled := strings.Contains(text, "Enabled") || strings.Contains(text, "已启用")
		if err != nil || !enabled {
			return false
		}
	}
	return true
}

// SetupFirewall adds inbound allow rules for the two ports. Requires
// elevation; without it netsh refuses and the error says so.
func SetupFirewall(tcpPort, udpPort int) error {
	for _, r := range []struct {
		proto string
		port  int
		desc  string
	}{
		{"TCP", tcpPort, "学生端连接教师端的 HTTP 端口"},
		{"UDP", udpPort, "学生端自动发现教师端使用的广播端口"},
	} {
		name := ruleName(r.proto, r.port)
		// Remove any stale rule for the same name first, so re-running after a
		// port change does not leave a rule pointing at the old port.
		_ = netsh("advfirewall", "firewall", "delete", "rule", "name="+name).Run()

		out, err := netsh("advfirewall", "firewall", "add", "rule",
			"name="+name,
			"description="+r.desc,
			"dir=in", "action=allow",
			"protocol="+r.proto,
			fmt.Sprintf("localport=%d", r.port),
			"profile=any",
		).CombinedOutput()
		if err != nil {
			return fmt.Errorf("添加防火墙规则「%s」失败: %w\n%s\n"+
				"（这一步需要管理员权限：右键 coderoom-server.exe → 以管理员身份运行）",
				name, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// FirewallHint is the one-liner shown when the rules are missing.
func FirewallHint(tcpPort, udpPort int) string {
	return fmt.Sprintf(
		"学生端可能连不上：Windows 防火墙默认拦截入站连接。\n"+
			"     以管理员身份运行一次即可放行 TCP %d 与 UDP %d：\n"+
			"       coderoom-server.exe -setup-firewall",
		tcpPort, udpPort)
}
