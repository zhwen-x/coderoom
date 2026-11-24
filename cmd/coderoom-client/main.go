// Command coderoom-client is the student-side application: 登录、查看题目、
// 内嵌编辑器写/粘贴代码、提交判题，并在后台定时上传代码与上报异常行为。
//
// It is a single portable executable. Everything the student types is written
// to disk locally before being uploaded, so neither a network outage nor a
// crash costs them their work.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"coderoom/internal/client"
)

func main() {
	// The Win32 message loop in RunWindow is thread-affine, so the main
	// goroutine must stay on its original OS thread.
	runtime.LockOSThread()

	dataDir := flag.String("data", defaultDataDir(), "本机数据目录（草稿、待上报事件、导出的答卷）")
	discoveryPort := flag.Int("discovery-port", 8099, "服务端自动发现使用的 UDP 端口")
	serverAddr := flag.String("server", "", "直接指定服务端地址，如 192.168.1.10:8080（跳过自动发现）")
	// Headless mode keeps the local API and background loops running without
	// opening a window. Used for diagnostics and automated testing.
	noWindow := flag.Bool("no-window", false, "不打开界面窗口，仅运行本地服务（诊断/测试用）")
	restore := flag.Bool("restore", false,
		"解除本机残留的考场管控（U盘/断网）后退出。考试异常中断时的手动恢复手段")
	watchdog := flag.Int("watchdog", 0,
		"内部使用：监视指定进程，其退出后立即解除本机管控")
	restoreIfGone := flag.Int("restore-if-gone", -1,
		"内部使用：由开机恢复任务调用；仅当应用管控的客户端已不存在时才恢复")
	flag.Parse()

	logFile, logger := openLogger(*dataDir)
	if logFile != nil {
		defer logFile.Close()
	}

	// Both of these run before anything else is set up: they need no server,
	// no login and no window, and the whole point of them is working when the
	// normal path did not.
	if *restore {
		if err := client.RestoreOnly(*dataDir, logger.Printf); err != nil {
			fatal(logger, "恢复失败: %v", err)
		}
		return
	}
	if *restoreIfGone >= 0 {
		if err := client.RestoreIfOwnerGone(*dataDir, *restoreIfGone, logger.Printf); err != nil {
			fatal(logger, "恢复失败: %v", err)
		}
		return
	}
	if *watchdog > 0 {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		client.WatchParent(ctx, *watchdog, *dataDir, logger.Printf)
		return
	}

	app, err := client.NewApp(*dataDir, *discoveryPort, logger)
	if err != nil {
		fatal(logger, "初始化失败: %v", err)
	}

	ln, baseURL, err := app.Listen()
	if err != nil {
		fatal(logger, "无法启动本地界面服务: %v", err)
	}
	logger.Printf("本地界面 %s", baseURL)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := app.Serve(ctx, ln); err != nil {
			logger.Printf("本地界面服务异常: %v", err)
		}
	}()
	// app.Run owns the final flush, so main has to wait for it rather than
	// returning the moment the window closes.
	background := make(chan struct{})
	go func() {
		defer close(background)
		app.Run(ctx)
	}()

	url := baseURL + "/"
	if *serverAddr != "" {
		// Connect before the UI loads, which is how a lab with broadcast
		// disabled would deploy the client. The address is also handed to the
		// UI so a failure here still shows the student what was tried.
		if err := app.Connect(ctx, *serverAddr); err != nil {
			logger.Printf("连接指定服务端 %s 失败: %v", *serverAddr, err)
		}
		url += "?server=" + *serverAddr
	}

	if *noWindow {
		fmt.Printf("\n  本地界面: %s\n  (已禁用窗口，按 Ctrl+C 退出)\n\n", url)
		<-ctx.Done()
	} else if err := client.RunWindow(ctx, url, "CodeRoom 学生端",
		filepath.Join(*dataDir, "webview")); err != nil {
		fatal(logger, "%v", err)
	}
	stop()

	// Without this wait the process exits while the last upload is still in
	// flight: the student's final code and the "client exiting" event stay on
	// disk, and the teacher's dashboard cannot tell a student who closed the
	// client properly from one who killed the process. The flush bounds itself
	// to a few seconds; the timeout here is only a guard against a wedged
	// goroutine holding the window-less process open.
	select {
	case <-background:
	case <-time.After(backgroundStopTimeout):
		logger.Printf("后台任务未能在 %s 内收尾，仍然退出", backgroundStopTimeout)
	}
}

// backgroundStopTimeout is deliberately a little longer than the client's own
// flush budget, so the flush decides when to give up and this only catches a
// goroutine that never finished at all.
const backgroundStopTimeout = 6 * time.Second

func defaultDataDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "coderoom-client-data"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), "coderoom-client-data")
}

// openLogger writes to a file next to the data dir so a student can be asked
// to send it, while still echoing to stdout for development.
func openLogger(dataDir string) (*os.File, *log.Logger) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, log.New(os.Stdout, "", log.LstdFlags)
	}
	f, err := os.OpenFile(filepath.Join(dataDir, "client.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, log.New(os.Stdout, "", log.LstdFlags)
	}
	return f, log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags)
}

func fatal(logger *log.Logger, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	logger.Println(msg)
	fmt.Fprintln(os.Stderr, "\n  错误: "+msg)
	os.Exit(1)
}
