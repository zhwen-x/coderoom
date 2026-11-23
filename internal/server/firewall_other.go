//go:build !windows

package server

import "fmt"

// Only Windows ships a firewall that blocks a new listener by default, so the
// development platforms report ready and do nothing.

func FirewallReady(tcpPort, udpPort int) bool { return true }

func SetupFirewall(tcpPort, udpPort int) error {
	return fmt.Errorf("仅 Windows 需要配置防火墙")
}

func FirewallHint(tcpPort, udpPort int) string { return "" }
