//go:build windows

package client

import (
	"context"
	"errors"
	"fmt"

	webview "github.com/jchv/go-webview2"
)

// ErrNoWebView2 means the WebView2 runtime is missing. Win10 1803+ and Win11
// ship it with Edge, so this is normally only hit on an old or stripped image.
var ErrNoWebView2 = errors.New(
	"未检测到 Microsoft Edge WebView2 运行时。请先安装发行包内的 " +
		"MicrosoftEdgeWebview2Setup.exe，或在有网络的机器上安装 WebView2 Evergreen Runtime 后重试")

// ErrWindowFailed covers the runtime being present but the window still not
// coming up.
var ErrWindowFailed = errors.New("无法创建程序窗口")

// createWindow isolates the library call because it does not fail cleanly.
//
// go-webview2 dereferences a nil controller inside its creation-completed
// callback when WebView2 refuses to start, so the process dies with a Go
// panic before NewWithOptions can return nil - the nil check below never got
// a chance to run. Observed on a machine with the runtime correctly installed
// but no desktop available to the session. Recovering turns a stack trace a
// student cannot act on into a message they can show their teacher.
func createWindow(title, dataPath string) (wv webview.WebView, err error) {
	defer func() {
		if r := recover(); r != nil {
			wv = nil
			err = fmt.Errorf("%w：WebView2 组件初始化失败。"+
				"请确认 Microsoft Edge 可以正常打开，必要时重装 WebView2 运行时。"+
				"（底层错误: %v）", ErrWindowFailed, r)
		}
	}()

	wv = webview.NewWithOptions(webview.WebViewOptions{
		// Debug=false also turns off the context menu and developer tools,
		// which is what we want for an exam client.
		Debug:     false,
		DataPath:  dataPath,
		AutoFocus: true,
		WindowOptions: webview.WindowOptions{
			Title:  title,
			Width:  1280,
			Height: 820,
			Center: true,
		},
	})
	if wv == nil {
		return nil, ErrNoWebView2
	}
	return wv, nil
}

// RunWindow shows the student UI in a WebView2 window and blocks until the
// window closes or ctx is cancelled.
//
// Must be called from the main goroutine with the OS thread locked: the Win32
// message loop is thread-affine.
func RunWindow(ctx context.Context, url, title, dataPath string) error {
	w, err := createWindow(title, dataPath)
	if err != nil {
		return err
	}
	defer w.Destroy()

	w.SetSize(1024, 680, webview.HintMin)
	w.Navigate(url)

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			w.Terminate()
		case <-done:
		}
	}()

	w.Run() // blocks on the Win32 message loop
	close(done)
	return nil
}
