//go:build windows

package client

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
)

// These tests must run on a real Windows machine. Everything they cover is
// either a hand-declared struct layout or an API whose behaviour cannot be
// checked by cross-compiling - which is exactly the category that produced
// every defect found in the M1 sandbox.

func TestDrivesFindsTheSystemDisk(t *testing.T) {
	p := &winProbe{}
	list, err := p.drives()
	if err != nil {
		t.Fatalf("drives: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("no drives at all; GetLogicalDrives returned nothing usable")
	}
	var sys *driveInfo
	for i := range list {
		t.Logf("%s %s ready=%v", list[i].Letter, list[i].Kind, list[i].Ready)
		if list[i].Letter == "C:" {
			sys = &list[i]
		}
	}
	if sys == nil {
		t.Fatal("C: not enumerated")
	}
	if sys.Kind != "fixed" {
		t.Errorf("C: reported as %q, want fixed", sys.Kind)
	}
	// A fixed disk is never probed for media, so this also checks that
	// driveReady short-circuits instead of calling GetVolumeInformationW.
	if !sys.Ready {
		t.Error("C: must always be ready")
	}
}

func TestProcessesIncludesThisTest(t *testing.T) {
	p := &winProbe{}
	list, err := p.processes()
	if err != nil {
		t.Fatalf("processes: %v", err)
	}
	if len(list) < 10 {
		t.Fatalf("only %d processes; the Toolhelp walk stopped early", len(list))
	}
	me := uint32(os.Getpid())
	var found string
	for _, pr := range list {
		if pr.PID == me {
			found = pr.Name
		}
	}
	if found == "" {
		t.Fatalf("this process (pid %d) is missing from the snapshot", me)
	}
	if !strings.HasSuffix(strings.ToLower(found), ".exe") {
		t.Errorf("process name looks truncated: %q", found)
	}
	// The same PID, resolved the other way: the foreground check relies on it.
	if byPID := processNameByPID(me); !strings.EqualFold(byPID, found) {
		t.Errorf("processNameByPID(%d) = %q, snapshot says %q", me, byPID, found)
	}
}

func TestForegroundDoesNotError(t *testing.T) {
	p := &winProbe{}
	fg, err := p.foreground()
	if err != nil {
		t.Fatalf("foreground: %v", err)
	}
	// Over SSH there is no interactive desktop, so an empty result is correct
	// and must not be reported as a failure - a monitor that errored here
	// would spam the teacher's dashboard on every machine at the lock screen.
	t.Logf("foreground: pid=%d process=%q title=%q", fg.PID, fg.Process, fg.Title)
}

// The MIB_TCPROW structs are declared by hand, field offset by field offset,
// and mix host-order PIDs with network-order addresses and ports. This test
// checks that decode against ground truth: a connection this process opens
// itself, whose PID, address and port are already known.
func TestConnectionsDecodesARealSocket(t *testing.T) {
	for _, tc := range []struct{ name, addr, wantIP string }{
		{"ipv4", "127.0.0.1:0", "127.0.0.1"},
		{"ipv6", "[::1]:0", "::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", tc.addr)
			if err != nil {
				t.Skipf("cannot listen on %s: %v", tc.addr, err)
			}
			defer ln.Close()

			done := make(chan net.Conn, 1)
			go func() {
				c, err := ln.Accept()
				if err == nil {
					done <- c
				}
			}()
			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			server := <-done
			defer server.Close()

			_, portStr, _ := net.SplitHostPort(ln.Addr().String())
			wantPort, _ := strconv.Atoi(portStr)

			rows, err := (&winProbe{}).connections()
			if err != nil {
				t.Fatalf("connections: %v", err)
			}
			me := uint32(os.Getpid())
			want := net.JoinHostPort(tc.wantIP, strconv.Itoa(wantPort))
			for _, r := range rows {
				if r.PID == me && r.Remote == want {
					return // decoded correctly: pid, address and port all match
				}
			}
			for _, r := range rows {
				if r.PID == me {
					t.Logf("own connection seen: %s (process %q)", r.Remote, r.Process)
				}
			}
			t.Fatalf("established connection to %s from pid %d not found in %d rows",
				want, me, len(rows))
		})
	}
}

// The process name must come back attached to the connection, since a remote
// address on its own tells an invigilator nothing about who is talking.
func TestConnectionsCarryProcessNames(t *testing.T) {
	rows, err := (&winProbe{}).connections()
	if err != nil {
		t.Fatalf("connections: %v", err)
	}
	named := 0
	for _, r := range rows {
		if r.Process != "" {
			named++
		}
	}
	if len(rows) > 0 && named == 0 {
		t.Fatalf("none of the %d connections resolved to a process", len(rows))
	}
	t.Logf("%d/%d connections carry a process name", named, len(rows))
}
