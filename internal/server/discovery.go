package server

import (
	"context"
	"encoding/json"
	"net"
	"time"
)

// DiscoveryProbe is what a client broadcasts to find the server, and
// DiscoveryReply is what comes back. Keeping this to a tiny UDP exchange means
// a student machine needs no configuration at all on a flat classroom LAN -
// the client still accepts a manually typed address when broadcast is blocked.
const DiscoveryProbe = "CODEROOM-DISCOVER-v1"

type DiscoveryReply struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	Name    string `json:"name"`
}

// serveDiscovery answers broadcast probes until ctx is cancelled. A failure
// here is not fatal: discovery is a convenience, and the client can always be
// pointed at an explicit address.
func (s *Server) serveDiscovery(ctx context.Context) {
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: s.cfg.DiscoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		s.logger.Printf("自动发现服务未启动 (端口 %d): %v；学生端可手动输入服务端地址",
			s.cfg.DiscoveryPort, err)
		return
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	reply, _ := json.Marshal(DiscoveryReply{
		Service: "coderoom",
		Port:    s.Port(),
		Version: ClientVersion,
		Name:    "CodeRoom 判题服务端",
	})

	buf := make([]byte, 256)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return // socket closed
		}
		if string(buf[:n]) != DiscoveryProbe {
			continue
		}
		if _, err := conn.WriteToUDP(reply, from); err != nil {
			s.logger.Printf("discovery reply to %s: %v", from, err)
		}
	}
}
