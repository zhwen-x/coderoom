// Package client is the student-side application: it finds the server, logs
// the student in, shows the paper, hosts the built-in editor, collects code on
// a timer, and reports behaviour events.
//
// Everything the student types is persisted locally before it is uploaded, so
// a network outage or a client crash never costs a student their work.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"coderoom/internal/protocol"
)

const (
	Version        = "0.1.0"
	requestTimeout = 15 * time.Second
)

// Upstream talks to the CodeRoom server.
type Upstream struct {
	addr  string // host:port
	token string
	hc    *http.Client
}

func NewUpstream(addr string) *Upstream {
	return &Upstream{
		addr: addr,
		hc:   &http.Client{Timeout: requestTimeout},
	}
}

func (u *Upstream) Addr() string      { return u.addr }
func (u *Upstream) SetToken(t string) { u.token = t }
func (u *Upstream) Token() string     { return u.token }

// APIError is a structured error from the server, so the UI can show the
// server's own Chinese explanation rather than a generic failure.
type APIError struct {
	Status int
	Code   string
	Detail string
}

func (e *APIError) Error() string {
	if e.Detail != "" {
		return e.Detail
	}
	return fmt.Sprintf("服务端返回 %d (%s)", e.Status, e.Code)
}

func (u *Upstream) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	endpoint := "http://" + u.addr + path
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if u.token != "" {
		req.Header.Set("Authorization", "Bearer "+u.token)
	}

	res, err := u.hc.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接服务端 %s: %w", u.addr, err)
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var er protocol.ErrorResponse
		_ = json.Unmarshal(raw, &er)
		return &APIError{Status: res.StatusCode, Code: er.Error, Detail: er.Detail}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

func (u *Upstream) ListExams(ctx context.Context) (*protocol.ExamListResponse, error) {
	var out protocol.ExamListResponse
	err := u.do(ctx, http.MethodGet, "/api/client/exams", nil, &out)
	return &out, err
}

func (u *Upstream) Login(ctx context.Context, req protocol.LoginRequest) (*protocol.LoginResponse, error) {
	var out protocol.LoginResponse
	if err := u.do(ctx, http.MethodPost, "/api/client/login", req, &out); err != nil {
		return nil, err
	}
	u.token = out.Token
	return &out, nil
}

func (u *Upstream) Paper(ctx context.Context) (*protocol.PaperResponse, error) {
	var out protocol.PaperResponse
	err := u.do(ctx, http.MethodGet, "/api/client/paper", nil, &out)
	return &out, err
}

func (u *Upstream) Snapshot(ctx context.Context, req protocol.SnapshotRequest) (*protocol.SnapshotResponse, error) {
	var out protocol.SnapshotResponse
	err := u.do(ctx, http.MethodPost, "/api/client/snapshot", req, &out)
	return &out, err
}

func (u *Upstream) Submit(ctx context.Context, req protocol.SubmitRequest) (*protocol.SubmitResponse, error) {
	var out protocol.SubmitResponse
	err := u.do(ctx, http.MethodPost, "/api/client/submit", req, &out)
	return &out, err
}

func (u *Upstream) Heartbeat(ctx context.Context, req protocol.HeartbeatRequest) (*protocol.HeartbeatResponse, error) {
	var out protocol.HeartbeatResponse
	err := u.do(ctx, http.MethodPost, "/api/client/heartbeat", req, &out)
	return &out, err
}

func (u *Upstream) SendEvents(ctx context.Context, req protocol.EventsRequest) error {
	return u.do(ctx, http.MethodPost, "/api/client/events", req, nil)
}

func (u *Upstream) HandIn(ctx context.Context) error {
	return u.do(ctx, http.MethodPost, "/api/client/handin", struct{}{}, nil)
}

func (u *Upstream) Results(ctx context.Context) (*protocol.ResultsResponse, error) {
	var out protocol.ResultsResponse
	err := u.do(ctx, http.MethodGet, "/api/client/results", nil, &out)
	return &out, err
}

// ---------------------------------------------------------------- discovery

// Discovered is one server found on the LAN.
type Discovered struct {
	Addr    string `json:"addr"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// DiscoverServers broadcasts a probe and collects replies. On a locked-down
// network broadcast may be dropped entirely; the UI always keeps a manual
// address box for that case, so a failure here is not an error state.
func DiscoverServers(ctx context.Context, discoveryPort int, wait time.Duration) ([]Discovered, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	probe := []byte(DiscoveryProbe)
	targets := broadcastTargets(discoveryPort)
	for _, t := range targets {
		_, _ = conn.WriteToUDP(probe, t)
	}

	deadline := time.Now().Add(wait)
	_ = conn.SetReadDeadline(deadline)

	seen := map[string]bool{}
	var out []Discovered
	buf := make([]byte, 1024)
	for {
		if ctx.Err() != nil {
			break
		}
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			break // deadline reached
		}
		var reply discoveryReply
		if json.Unmarshal(buf[:n], &reply) != nil || reply.Service != "coderoom" {
			continue
		}
		addr := fmt.Sprintf("%s:%d", from.IP.String(), reply.Port)
		if seen[addr] {
			continue
		}
		seen[addr] = true
		out = append(out, Discovered{Addr: addr, Name: reply.Name, Version: reply.Version})
	}
	return out, nil
}

// DiscoveryProbe and discoveryReply mirror the server side. They are
// duplicated rather than imported so the client binary does not link the
// server package (and its SQLite driver).
const DiscoveryProbe = "CODEROOM-DISCOVER-v1"

type discoveryReply struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	Version string `json:"version"`
	Name    string `json:"name"`
}

// broadcastTargets returns the global broadcast address plus each interface's
// own subnet broadcast. Some Windows configurations drop 255.255.255.255 but
// deliver the subnet-directed form, so both are tried.
func broadcastTargets(port int) []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: port}}
	ifaces, err := net.Interfaces()
	if err != nil {
		return targets
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ip := ipn.IP.To4()
			mask := net.IP(ipn.Mask).To4()
			if mask == nil {
				continue
			}
			bc := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				bc[i] = ip[i] | ^mask[i]
			}
			targets = append(targets, &net.UDPAddr{IP: bc, Port: port})
		}
	}
	return targets
}

// NormalizeAddr accepts what a student might type - "192.168.1.5",
// "192.168.1.5:8080", "http://192.168.1.5:8080" - and returns host:port.
func NormalizeAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("地址为空")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("地址格式错误: %w", err)
		}
		s = u.Host
	}
	if !strings.Contains(s, ":") {
		s += ":8080"
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("地址格式错误: %w", err)
	}
	if host == "" {
		return "", fmt.Errorf("缺少主机地址")
	}
	return net.JoinHostPort(host, port), nil
}
