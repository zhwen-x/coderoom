//go:build windows

package client

import (
	"context"
	"errors"

	webview "github.com/jchv/go-webview2"
)

// ErrNoWebView2 means the WebView2 runtime is missing. Win10 1803+ and Win11
// ship it with Edge, so this is normally only hit on an old or stripped image.
var ErrNoWebView2 = errors.New(
	"未检测到 Microsoft Edge WebView2 运行时。请先安装发行包内的 " +
		"MicrosoftEdgeWebview2Setup.exe，或在有网络的机器上安装 WebView2 Evergreen Runtime 后重试")

// RunWindow shows the student UI in a WebView2 window and blocks until the
// window closes or ctx is cancelled.
//
// Must be called from the main goroutine with the OS thread locked: the Win32
// message loop is thread-affine.
func RunWindow(ctx context.Context, url, title, dataPath string) error {
	w := webview.NewWithOptions(webview.WebViewOptions{
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
	if w == nil {
		return ErrNoWebView2
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
