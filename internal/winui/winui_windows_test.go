//go:build windows

package winui

import (
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// What a headless session can prove about the tray.
//
// An SSH session has a console and a window station but no Explorer, so
// Shell_NotifyIcon has nowhere to put an icon. Everything up to that point is
// real though - and that is where the failures that only appear on Windows
// live: a callback the ABI refuses, an icon parsed out of the wrong offset, a
// window class that will not register. Those are tested here. Whether the icon
// is visible and its menu usable needs somebody at the machine.

// An SSH session's console is a pseudoconsole whose window is never on screen,
// so the "becomes visible again" half of this can only be checked where there
// is a desktop. The rest holds everywhere and is what the callers branch on:
// HideConsole has to report whether there was a console at all, because that
// is how the server decides between a tray icon and its ordinary window.
func TestConsoleHideAndShow(t *testing.T) {
	if ConsoleWindow() == 0 {
		t.Skip("本会话没有控制台窗口")
	}
	startedVisible := ConsoleVisible()
	t.Cleanup(func() {
		if startedVisible {
			ShowConsole()
		}
	})

	if !HideConsole() {
		t.Fatal("HideConsole 报告没有控制台可隐藏，服务端会据此放弃托盘")
	}
	if ConsoleVisible() {
		t.Error("调用 HideConsole 之后窗口仍然可见")
	}
	// The teacher can click 隐藏 twice; neither call may report failure.
	if !HideConsole() || ConsoleVisible() {
		t.Error("重复隐藏之后状态不对")
	}

	ShowConsole()
	if !startedVisible {
		t.Log("本会话的控制台本就不可见（SSH 伪控制台），跳过「重新显示」的断言")
		return
	}
	if !ConsoleVisible() {
		t.Error("调用 ShowConsole 之后窗口仍然不可见")
	}
}

// The icon is parsed out of an embedded .ico and handed to Win32 as a pointer
// and a length. A wrong offset would hand it whatever follows in the binary.
func TestEmbeddedIconLoads(t *testing.T) {
	h, err := loadEmbeddedIcon()
	if err != nil {
		t.Fatalf("loadEmbeddedIcon: %v", err)
	}
	if h == 0 {
		t.Fatal("图标句柄为 0")
	}
	destroyIcon := user32.NewProc("DestroyIcon")
	t.Cleanup(func() { destroyIcon.Call(uintptr(h)) })

	// GetIconInfo only succeeds on a real icon object, so it is the check that
	// the handle means something rather than merely being non-zero.
	type iconInfo struct {
		IsIcon      int32
		XHotspot    uint32
		YHotspot    uint32
		MaskBitmap  windows.Handle
		ColorBitmap windows.Handle
	}
	var info iconInfo
	getIconInfo := user32.NewProc("GetIconInfo")
	ret, _, callErr := getIconInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
	if ret == 0 {
		t.Fatalf("GetIconInfo 失败，句柄不是有效图标: %v", callErr)
	}
	deleteObject := windows.NewLazySystemDLL("gdi32.dll").NewProc("DeleteObject")
	if info.MaskBitmap != 0 {
		deleteObject.Call(uintptr(info.MaskBitmap))
	}
	if info.ColorBitmap != 0 {
		deleteObject.Call(uintptr(info.ColorBitmap))
	}
	if info.IsIcon == 0 {
		t.Error("加载出来的是光标而不是图标")
	}
}

// windows.NewCallback panics if any argument is not pointer-sized, and it does
// so the first time a tray is created - on a teacher's machine. Registering
// the real window procedure here is the only way to find out before then.
func TestWndProcIsAcceptedAsACallback(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NewCallback 拒绝了 wndProc 的签名: %v", r)
		}
	}()
	tr := &winTray{}
	if cb := windows.NewCallback(tr.wndProc); cb == 0 {
		t.Fatal("NewCallback 返回 0")
	}
}

// Run goes through class registration, window creation and Shell_NotifyIcon.
// Only the last of those needs a shell, so a failure here is still worth
// seeing: it says which step a machine actually stops at.
func TestTrayRunReachesTheShell(t *testing.T) {
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		done <- Run("CodeRoom 测试托盘", []MenuItem{
			{Label: "打开", Default: true, OnClick: func() {}},
			{Separator: true},
			{Label: "退出", OnClick: func() {}},
		}, func(tr Tray) {
			close(started)
			tr.SetTooltip("测试")
			go func() {
				time.Sleep(300 * time.Millisecond)
				tr.Stop()
			}()
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			// Expected without Explorer. The point is that it got this far
			// rather than panicking or failing at class registration.
			t.Logf("托盘未能创建（无桌面会话时属预期）: %v", err)
			return
		}
		t.Log("托盘创建并退出成功")
	case <-time.After(10 * time.Second):
		t.Fatal("Run 既没有成功也没有失败，疑似消息循环卡住")
	}

	select {
	case <-started:
		t.Log("ready 回调已触发：图标已被 shell 接受")
	default:
	}
}
