//go:build windows

package client

import (
	"encoding/binary"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows half of the behaviour monitor: four read-only queries against
// the OS. Nothing here changes any machine state - that is M4's job, and it
// needs a watchdog this milestone deliberately does not have yet.
//
// Everything runs with the student's own rights. No elevation, no driver, no
// service: a monitor that needed administrator would conflict with the
// install-free requirement, and three of these four checks do not need it.

func newSysProbe() sysProbe { return &winProbe{} }

type winProbe struct{}

func (p *winProbe) caps() probeCaps {
	return probeCaps{Drives: true, Processes: true, Foreground: true, Connections: true}
}

// ------------------------------------------------------------------- drives

var (
	modkernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procGetVolumeInfoW   = modkernel32.NewProc("GetVolumeInformationW")
	moduser32            = windows.NewLazySystemDLL("user32.dll")
	procGetWindowTextW   = moduser32.NewProc("GetWindowTextW")
	modiphlpapi          = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTb = modiphlpapi.NewProc("GetExtendedTcpTable")
)

func (p *winProbe) drives() ([]driveInfo, error) {
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("GetLogicalDrives: %w", err)
	}
	var out []driveInfo
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		letter := string(rune('A'+i)) + ":"
		root, err := windows.UTF16PtrFromString(letter + `\`)
		if err != nil {
			continue
		}
		kind := driveKindName(windows.GetDriveType(root))
		out = append(out, driveInfo{
			Letter: letter,
			Kind:   kind,
			Ready:  driveReady(kind, root),
		})
	}
	return out, nil
}

func driveKindName(t uint32) string {
	switch t {
	case windows.DRIVE_REMOVABLE:
		return "removable"
	case windows.DRIVE_FIXED:
		return "fixed"
	case windows.DRIVE_REMOTE:
		return "remote"
	case windows.DRIVE_CDROM:
		return "cdrom"
	case windows.DRIVE_RAMDISK:
		return "ramdisk"
	default:
		return "unknown"
	}
}

// driveReady reports whether there is media behind a drive letter.
//
// Only removable media is actually probed. A fixed disk is always there, and a
// network drive is emphatically not worth asking about: GetVolumeInformationW
// on a dead SMB share blocks for the SMB timeout, which would stall this
// goroutine for tens of seconds once a second.
func driveReady(kind string, root *uint16) bool {
	switch kind {
	case "removable", "cdrom":
	case "unknown":
		return false // DRIVE_NO_ROOT_DIR and friends: nothing mounted
	default:
		return true
	}
	// GetVolumeInformationW fails with ERROR_NOT_READY on an empty slot. It is
	// the cheapest "is a card in the reader" question Windows answers; the
	// alternative (opening the volume) spins the device up.
	var (
		nameBuf [261]uint16
		serial  uint32
		maxComp uint32
		flags   uint32
		fsBuf   [261]uint16
	)
	ret, _, _ := procGetVolumeInfoW.Call(
		uintptr(unsafe.Pointer(root)),
		uintptr(unsafe.Pointer(&nameBuf[0])), uintptr(len(nameBuf)),
		uintptr(unsafe.Pointer(&serial)),
		uintptr(unsafe.Pointer(&maxComp)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&fsBuf[0])), uintptr(len(fsBuf)),
	)
	return ret != 0
}

// ---------------------------------------------------------------- processes

func (p *winProbe) processes() ([]procInfo, error) {
	return snapshotProcesses()
}

// snapshotProcesses enumerates every process the student can see. Toolhelp is
// used rather than opening each process in turn: it needs no per-process
// handle, so it works without elevation and cannot be refused one name at a
// time.
func snapshotProcesses() ([]procInfo, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil, fmt.Errorf("Process32First: %w", err)
	}
	out := make([]procInfo, 0, 256)
	for {
		out = append(out, procInfo{
			PID:  entry.ProcessID,
			Name: windows.UTF16ToString(entry.ExeFile[:]),
		})
		if err := windows.Process32Next(snap, &entry); err != nil {
			break // ERROR_NO_MORE_FILES ends the walk
		}
	}
	return out, nil
}

// processNameByPID resolves one PID. QueryFullProcessImageName is tried first
// because it costs a single handle; it is refused for processes running as
// another user (an elevated window), and the Toolhelp walk is the fallback
// that always works.
func processNameByPID(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err == nil {
		defer windows.CloseHandle(h)
		buf := make([]uint16, windows.MAX_PATH)
		size := uint32(len(buf))
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil {
			return filepath.Base(windows.UTF16ToString(buf[:size]))
		}
	}
	list, err := snapshotProcesses()
	if err != nil {
		return ""
	}
	for _, pr := range list {
		if pr.PID == pid {
			return pr.Name
		}
	}
	return ""
}

// --------------------------------------------------------------- foreground

func (p *winProbe) foreground() (fgInfo, error) {
	hwnd := windows.GetForegroundWindow()
	if hwnd == 0 {
		// No foreground window at all: the lock screen, or the moment between
		// one window closing and the next taking over. Not an error.
		return fgInfo{}, nil
	}
	var pid uint32
	if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil {
		return fgInfo{}, fmt.Errorf("GetWindowThreadProcessId: %w", err)
	}
	return fgInfo{
		PID:     pid,
		Process: processNameByPID(pid),
		Title:   windowText(hwnd),
	}, nil
}

func windowText(hwnd windows.HWND) string {
	buf := make([]uint16, 512)
	n, _, _ := procGetWindowTextW.Call(
		uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// ----------------------------------------------------------------- network

const (
	tcpTableOwnerPIDAll = 5 // TCP_TABLE_CLASS: TCP_TABLE_OWNER_PID_ALL
	mibTCPStateEstab    = 5 // MIB_TCP_STATE_ESTAB
	rowSizeV4           = 24
	rowSizeV6           = 56
)

// connections lists established TCP connections with the owning PID.
//
// Both address families are queried. On a dual-stack lab network a browser
// will often prefer IPv6, and asking only for IPv4 would leave a blind spot
// that looks identical to a quiet machine.
func (p *winProbe) connections() ([]connInfo, error) {
	v4, err4 := tcpTable(windows.AF_INET)
	v6, err6 := tcpTable(windows.AF_INET6)
	if err4 != nil && err6 != nil {
		return nil, err4
	}
	rows := append(v4, v6...)
	if len(rows) == 0 {
		return nil, nil
	}

	// One process walk for the whole table: resolving names per row would open
	// a handle per connection, and a busy machine has hundreds.
	names := map[uint32]string{}
	if list, err := snapshotProcesses(); err == nil {
		for _, pr := range list {
			names[pr.PID] = pr.Name
		}
	}
	for i := range rows {
		rows[i].Process = names[rows[i].PID]
	}
	return rows, nil
}

// tcpTable calls GetExtendedTcpTable and decodes the rows byte by byte.
//
// The decode is deliberately explicit rather than a cast to a Go struct: the
// MIB rows mix host-order PIDs with network-order addresses and ports, and
// reading each field at its own offset makes that visible instead of hiding it
// behind a byte-swap that is easy to get backwards.
func tcpTable(family uint32) ([]connInfo, error) {
	var size uint32
	// First call sizes the buffer; the table can grow between the two calls,
	// so a short retry loop is the documented usage.
	for attempt := 0; attempt < 4; attempt++ {
		var base *byte
		buf := make([]byte, size)
		if size > 0 {
			base = &buf[0]
		}
		ret, _, _ := procGetExtendedTcpTb.Call(
			uintptr(unsafe.Pointer(base)), uintptr(unsafe.Pointer(&size)),
			0, uintptr(family), tcpTableOwnerPIDAll, 0)
		switch ret {
		case 0:
			return decodeTCPTable(buf[:size], family), nil
		case uintptr(windows.ERROR_INSUFFICIENT_BUFFER):
			continue // size now holds what is needed
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable(af=%d): %w",
				family, syscall.Errno(ret))
		}
	}
	return nil, fmt.Errorf("GetExtendedTcpTable(af=%d): 表持续增长，放弃", family)
}

func decodeTCPTable(buf []byte, family uint32) []connInfo {
	if len(buf) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(buf[:4]))
	rowSize, addrLen := rowSizeV4, 4
	if family == windows.AF_INET6 {
		rowSize, addrLen = rowSizeV6, 16
	}
	out := make([]connInfo, 0, n)
	for i := 0; i < n; i++ {
		off := 4 + i*rowSize
		if off+rowSize > len(buf) {
			break
		}
		row := buf[off : off+rowSize]

		var remoteIP net.IP
		var remotePort uint16
		var state, pid uint32
		if family == windows.AF_INET6 {
			// ucLocalAddr[16] dwLocalScopeId dwLocalPort
			// ucRemoteAddr[16] dwRemoteScopeId dwRemotePort dwState dwOwningPid
			remoteIP = net.IP(append([]byte(nil), row[24:24+addrLen]...))
			remotePort = binary.BigEndian.Uint16(row[44:46])
			state = binary.LittleEndian.Uint32(row[48:52])
			pid = binary.LittleEndian.Uint32(row[52:56])
		} else {
			// dwState dwLocalAddr dwLocalPort dwRemoteAddr dwRemotePort dwOwningPid
			state = binary.LittleEndian.Uint32(row[0:4])
			remoteIP = net.IP(append([]byte(nil), row[12:12+addrLen]...))
			remotePort = binary.BigEndian.Uint16(row[16:18])
			pid = binary.LittleEndian.Uint32(row[20:24])
		}
		// Only established connections count. A half-open or closing socket
		// says nothing about where the machine is actually talking.
		if state != mibTCPStateEstab {
			continue
		}
		out = append(out, connInfo{
			PID:    pid,
			Remote: net.JoinHostPort(remoteIP.String(), strconv.Itoa(int(remotePort))),
		})
	}
	return out
}
