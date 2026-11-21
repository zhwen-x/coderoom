//go:build !windows

package client

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
)

// RunWindow is the development fallback for non-Windows hosts: the UI opens in
// the default browser instead of an embedded WebView2 window, and the process
// stays alive until it is interrupted.
//
// This exists so the whole client - state machine, local API, UI, offline
// queue - can be exercised on the macOS development machine. Only the window
// host differs on Windows.
func RunWindow(ctx context.Context, url, title, dataPath string) error {
	fmt.Printf("\n  [开发模式] 当前系统非 Windows，改用默认浏览器打开界面:\n     %s\n\n", url)

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("  自动打开浏览器失败 (%v)，请手动访问上面的地址。\n", err)
	}

	<-ctx.Done()
	return nil
}
