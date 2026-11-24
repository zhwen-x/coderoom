//go:build windows

package winui

import (
	_ "embed"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed coderoom.ico
var iconData []byte

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetConsoleWindow         = kernel32.NewProc("GetConsoleWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procSetForegroundWin         = user32.NewProc("SetForegroundWindow")
	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
)

const (
	swHide    = 0
	swShow    = 5
	swRestore = 9

	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmRButtonUp     = 0x0205
	wmLButtonDblClk = 0x0203
	wmApp           = 0x8000

	// wmTrayCallback is the private message Shell_NotifyIcon sends us for
	// mouse activity on the icon.
	wmTrayCallback = wmApp + 1
	// wmTrayStop asks the message loop to shut down from another goroutine.
	wmTrayStop = wmApp + 2

	nimAdd    = 0x0
	nimModify = 0x1
	nimDelete = 0x2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	mfString    = 0x0000
	mfSeparator = 0x0800

	firstMenuID = 100
)

type point struct{ X, Y int32 }

type msg struct {
	HWnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// notifyIconData is the NOTIFYICONDATAW layout for Windows 2000 and later.
// Only the fields up to szInfoTitle are used; the rest keeps the struct the
// size the API expects.
type notifyIconData struct {
	Size             uint32
	Wnd              windows.HWND
	ID               uint32
	Flags            uint32
	CallbackMessage  uint32
	Icon             windows.Handle
	Tip              [128]uint16
	State            uint32
	StateMask        uint32
	Info             [256]uint16
	TimeoutOrVersion uint32
	InfoTitle        [64]uint16
	InfoFlags        uint32
	GUIDItem         windows.GUID
	BalloonIcon      windows.Handle
}

// ------------------------------------------------------------------ console

// ConsoleWindow reports whether this process owns a console window at all.
// A process started from Explorer does; one started by the scheduler may not.
func ConsoleWindow() windows.HWND {
	h, _, _ := procGetConsoleWindow.Call()
	return windows.HWND(h)
}

// HideConsole takes the console window off the screen and the taskbar.
//
// The console is hidden rather than never created: building with
// -H windowsgui would remove it entirely, but then `-restore` - the documented
// manual escape from a stuck lock - would print its result to nowhere, and a
// teacher running it would have no idea whether it worked. Keeping a real
// console and hiding it costs one brief flash at startup and keeps every
// diagnostic path working exactly as it reads.
func HideConsole() bool {
	hwnd := ConsoleWindow()
	if hwnd == 0 {
		return false
	}
	procShowWindow.Call(uintptr(hwnd), swHide)
	return true
}

// ShowConsole brings the console window back and raises it.
func ShowConsole() {
	hwnd := ConsoleWindow()
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(uintptr(hwnd), swRestore)
	procShowWindow.Call(uintptr(hwnd), swShow)
	procSetForegroundWin.Call(uintptr(hwnd))
}

// ConsoleVisible reports whether the console window is currently on screen.
func ConsoleVisible() bool {
	hwnd := ConsoleWindow()
	if hwnd == 0 {
		return false
	}
	v, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	return v != 0
}

// --------------------------------------------------------------------- tray

type winTray struct {
	hwnd  windows.HWND
	icon  windows.Handle
	items []MenuItem
	// taskbarCreated is the broadcast Explorer sends after restarting, which
	// is when every tray icon has to be added again or it is simply gone.
	taskbarCreated uint32

	mu      sync.Mutex
	tooltip string
	stopped bool
}

// Run puts a tray icon on screen and services its messages until Stop is
// called or the menu quits.
//
// It must be called from a goroutine locked to its own OS thread: a window and
// its message loop belong to the thread that created the window, and Go will
// otherwise move the goroutine between threads and the loop will simply stop
// receiving anything. Run does the locking itself so callers cannot get it
// wrong, which is why it blocks rather than returning a handle.
func Run(title string, items []MenuItem, ready func(Tray)) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t := &winTray{items: items, tooltip: title}

	icon, err := loadEmbeddedIcon()
	if err != nil {
		return err
	}
	t.icon = icon

	className, _ := windows.UTF16PtrFromString("CodeRoomTrayWindow")
	wndProc := windows.NewCallback(t.wndProc)
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		return fmt.Errorf("GetModuleHandleEx: %w", err)
	}
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   wndProc,
		Instance:  inst,
		ClassName: className,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return fmt.Errorf("RegisterClassEx: %w", err)
	}

	// A message-only window: never shown, exists purely to receive the tray
	// icon's callbacks.
	windowName, _ := windows.UTF16PtrFromString(title)
	hwnd, _, err := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(windowName)),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		return fmt.Errorf("CreateWindowEx: %w", err)
	}
	t.hwnd = windows.HWND(hwnd)

	if name, err := windows.UTF16PtrFromString("TaskbarCreated"); err == nil {
		id, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(name)))
		t.taskbarCreated = uint32(id)
	}

	if err := t.notify(nimAdd); err != nil {
		procDestroyWindow.Call(hwnd)
		return err
	}
	if ready != nil {
		ready(t)
	}

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0 = WM_QUIT, -1 = error
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	t.notify(nimDelete)
	procDestroyWindow.Call(uintptr(t.hwnd))
	return nil
}

// wndProc takes everything as uintptr on purpose: windows.NewCallback requires
// every argument to be pointer-sized, and a uint32 here would panic the moment
// the callback is registered - on the real desktop, nowhere else.
func (t *winTray) wndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	switch uint32(message) {
	case wmTrayCallback:
		switch uint32(lparam) {
		case wmRButtonUp:
			t.showMenu()
		case wmLButtonDblClk:
			for _, it := range t.items {
				if it.Default && it.OnClick != nil {
					go it.OnClick()
					break
				}
			}
		}
		return 0
	case wmCommand:
		if id := int(wparam&0xffff) - firstMenuID; id >= 0 && id < len(t.items) {
			if fn := t.items[id].OnClick; fn != nil {
				// On its own goroutine: a handler that blocks would otherwise
				// freeze the message loop and wedge the icon.
				go fn()
			}
		}
		return 0
	case wmTrayStop, wmClose, wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	if t.taskbarCreated != 0 && uint32(message) == t.taskbarCreated {
		t.notify(nimAdd)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, message, wparam, lparam)
	return ret
}

func (t *winTray) showMenu() {
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hmenu)

	for i, it := range t.items {
		if it.Separator {
			procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
			continue
		}
		label, err := windows.UTF16PtrFromString(it.Label)
		if err != nil {
			continue
		}
		procAppendMenuW.Call(hmenu, mfString,
			uintptr(firstMenuID+i), uintptr(unsafe.Pointer(label)))
	}

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu refuses to close when the user clicks elsewhere,
	// which is a documented quirk of tray menus.
	procSetForegroundWin.Call(uintptr(t.hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(hmenu, tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(t.hwnd), 0)
	if cmd != 0 {
		procPostMessageW.Call(uintptr(t.hwnd), wmCommand, cmd, 0)
	}
}

func (t *winTray) data() *notifyIconData {
	d := &notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             t.hwnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayCallback,
		Icon:            t.icon,
	}
	t.mu.Lock()
	copyUTF16(d.Tip[:], t.tooltip)
	t.mu.Unlock()
	return d
}

func (t *winTray) notify(action uint32) error {
	d := t.data()
	ret, _, err := procShellNotifyIconW.Call(uintptr(action), uintptr(unsafe.Pointer(d)))
	if ret == 0 && action != nimDelete {
		return fmt.Errorf("Shell_NotifyIcon(%d): %w", action, err)
	}
	return nil
}

func (t *winTray) SetTooltip(s string) {
	t.mu.Lock()
	t.tooltip = s
	t.mu.Unlock()
	t.notify(nimModify)
}

func (t *winTray) Notify(title, body string) {
	d := t.data()
	d.Flags |= nifInfo
	copyUTF16(d.InfoTitle[:], title)
	copyUTF16(d.Info[:], body)
	d.InfoFlags = 0x1 // NIIF_INFO
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(d)))
}

func (t *winTray) Stop() {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	t.mu.Unlock()
	procPostMessageW.Call(uintptr(t.hwnd), wmTrayStop, 0, 0)
}

// copyUTF16 writes s into a fixed-size UTF-16 field, truncating rather than
// overflowing and always leaving the terminating zero.
func copyUTF16(dst []uint16, s string) {
	for i := range dst {
		dst[i] = 0
	}
	u := windows.StringToUTF16(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	copy(dst, u)
}

// loadEmbeddedIcon turns the embedded .ico into an HICON without writing it to
// disk. CreateIconFromResourceEx wants the icon *image* rather than the file,
// so the directory at the front is parsed here to find the best entry.
func loadEmbeddedIcon() (windows.Handle, error) {
	offset, size, err := bestIconImage(iconData, 32)
	if err != nil {
		return 0, err
	}
	h, _, callErr := procCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&iconData[offset])), uintptr(size),
		1,          // fIcon: an icon rather than a cursor
		0x00030000, // dwVer: the only value this API accepts
		32, 32, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateIconFromResourceEx: %w", callErr)
	}
	return windows.Handle(h), nil
}
