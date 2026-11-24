//go:build windows

package client

import (
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
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

// driveReady is the single decision that determines whether a USB stick is
// ever noticed, and it rests on a hand-rolled eight-argument call to
// GetVolumeInformationW. Get the argument order or the return convention wrong
// and every removable drive reports as an empty slot: no event, ever, with
// nothing in any log to say so.
//
// C: is deliberately passed in as "removable" to force that call on a volume
// whose answer is already known. Testing it on an actual USB stick would need
// someone standing at the machine.
func TestDriveReadyProbesRemovableMedia(t *testing.T) {
	root, err := windows.UTF16PtrFromString(`C:\`)
	if err != nil {
		t.Fatal(err)
	}
	if !driveReady("removable", root) {
		t.Error("GetVolumeInformationW reports no media on the system volume; " +
			"a real USB stick would be read as an empty slot and never raise an event")
	}

	// The other branch: a letter with nothing behind it must come back false,
	// or an empty card-reader slot would look like an insertion on every poll.
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		t.Fatal(err)
	}
	spare := ""
	for i := 25; i >= 0; i-- { // from Z: down, the least likely to be in use
		if mask&(1<<uint(i)) == 0 {
			spare = string(rune('A'+i)) + ":"
			break
		}
	}
	if spare == "" {
		t.Skip("every drive letter is in use")
	}
	empty, err := windows.UTF16PtrFromString(spare + `\`)
	if err != nil {
		t.Fatal(err)
	}
	if driveReady("removable", empty) {
		t.Errorf("%s has nothing mounted but reports as ready", spare)
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

// An SSH session has no interactive desktop, so GetForegroundWindow returns
// nothing there and the test above cannot reach the code that reads a window's
// title and owner. This creates a window of its own - invisible, since
// WS_VISIBLE is not set - so that GetWindowTextW and GetWindowThreadProcessId
// are checked against a title and a PID already known.
//
// The title is deliberately non-ASCII: real window titles in this lab are
// Chinese ("答案.txt - 记事本"), and a broken UTF-16 round trip would turn the
// most useful piece of evidence the foreground monitor collects into mojibake.
func TestWindowTextReadsARealTitle(t *testing.T) {
	var (
		procCreateWindowExW = moduser32.NewProc("CreateWindowExW")
		procDestroyWindow   = moduser32.NewProc("DestroyWindow")
	)
	const title = "CodeRoom 前台窗口测试 ✓"
	// STATIC is pre-registered by the system, so no window class of our own
	// has to be registered first.
	class, err := windows.UTF16PtrFromString("STATIC")
	if err != nil {
		t.Fatal(err)
	}
	text, err := windows.UTF16PtrFromString(title)
	if err != nil {
		t.Fatal(err)
	}
	hwnd, _, callErr := procCreateWindowExW.Call(
		0, // dwExStyle
		uintptr(unsafe.Pointer(class)),
		uintptr(unsafe.Pointer(text)),
		0,            // dwStyle: not WS_VISIBLE, nothing appears on screen
		0, 0, 10, 10, // x, y, width, height
		0, 0, 0, 0, // parent, menu, instance, param
	)
	if hwnd == 0 {
		t.Skipf("CreateWindowExW unavailable in this session: %v", callErr)
	}
	defer procDestroyWindow.Call(hwnd)

	if got := windowText(windows.HWND(hwnd)); got != title {
		t.Errorf("windowText = %q, want %q", got, title)
	}
	// The other half of foreground(): a window maps back to its owning process.
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(windows.HWND(hwnd), &pid); err != nil {
		t.Fatalf("GetWindowThreadProcessId: %v", err)
	}
	if pid != uint32(os.Getpid()) {
		t.Errorf("window owner pid = %d, want this process %d", pid, os.Getpid())
	}
	if name := processNameByPID(pid); !strings.Contains(strings.ToLower(name), "client") {
		t.Errorf("owner resolved to %q, expected this test binary", name)
	}
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
