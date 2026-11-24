// Command coderoom-server is the teacher-side service:题库管理、组卷分发、
// 代码收取、统一判题、成绩汇总。
//
// It is a single executable with the web console embedded, so deployment is
// "unzip and double-click" - no installer, no runtime, no database to set up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"coderoom/internal/server"
	"coderoom/internal/store"
	"coderoom/internal/winui"
)

func main() {
	defaultData := defaultDataDir()

	addr := flag.String("addr", "", "监听地址，如 :8080（留空使用配置文件中的值）")
	dataDir := flag.String("data", defaultData, "数据目录（数据库、测试数据、学生代码）")
	workers := flag.Int("workers", 0, "判题并发数（留空使用配置文件中的值）")
	openUI := flag.Bool("open", true, "启动后自动在浏览器中打开教师端")
	setupFirewall := flag.Bool("setup-firewall", false,
		"添加 Windows 防火墙入站规则后退出（需以管理员身份运行）")
	console := flag.Bool("console", false,
		"始终保留运行窗口，不最小化到托盘")
	flag.Parse()

	logger := log.New(os.Stdout, "", log.LstdFlags)

	st, err := store.Open(*dataDir)
	if err != nil {
		logger.Fatalf("打开数据目录失败 (%s): %v", *dataDir, err)
	}
	defer st.Close()

	cfg := server.LoadConfig(*dataDir)
	if *addr != "" {
		cfg.Addr = *addr
	}
	if *workers > 0 {
		cfg.Workers = *workers
	}

	baseDir := executableDir()
	srv, err := server.New(st, cfg, baseDir, logger)
	if err != nil {
		logger.Fatalf("初始化服务端失败: %v", err)
	}

	// Windows blocks inbound connections to a new executable by default: the
	// server listens, and every student machine times out. One elevated run
	// of this flag fixes it.
	if *setupFirewall {
		if err := server.SetupFirewall(srv.Port(), cfg.DiscoveryPort); err != nil {
			logger.Fatalf("%v", err)
		}
		fmt.Println("\n  已添加防火墙入站规则（按程序授权，改端口后无需重新添加）。")
		fmt.Println("  现在学生端应该可以连上了。")
		return
	}

	printBanner(srv, *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *openUI {
		go openBrowser(srv.AdminURL())
	}

	// The server runs for a whole morning on the teacher's machine, so by
	// default it gets out of the way: the window is hidden and a tray icon
	// takes its place. The window is hidden rather than never created, which
	// is what lets "显示运行窗口" bring back the banner and the judge log
	// exactly as they were.
	if !*console && runTray(ctx, stop, srv, logger) {
		return
	}

	if err := srv.Run(ctx); err != nil {
		logger.Fatalf("服务端异常退出: %v", err)
	}
	logger.Println("服务端已停止")
}

// runTray serves from the background with a tray icon in front. It reports
// whether it took over; false means the caller should run normally, which is
// what happens on a machine with no tray to put an icon in.
func runTray(ctx context.Context, stop context.CancelFunc,
	srv *server.Server, logger *log.Logger) bool {

	if !winui.HideConsole() {
		return false // no console window: nothing to minimise, nothing gained
	}

	done := make(chan error, 1)
	go func() {
		done <- srv.Run(ctx)
		// Cancelling here is what removes the tray icon when the server stops
		// by itself - a port already in use, say. Without it the teacher would
		// be left with an icon that looks like a running server.
		stop()
	}()

	addr := "本机"
	if ips := server.LocalIPs(); len(ips) > 0 {
		addr = fmt.Sprintf("%s:%d", ips[0], srv.Port())
	}

	var tray winui.Tray
	items := []winui.MenuItem{
		{Label: "打开教师端", Default: true, OnClick: func() {
			openBrowser(srv.AdminURL())
		}},
		{Label: "显示运行窗口（含管理口令）", OnClick: func() {
			winui.ShowConsole()
		}},
		{Label: "隐藏运行窗口", OnClick: func() {
			winui.HideConsole()
		}},
		{Separator: true},
		{Label: "退出", OnClick: func() {
			stop()
			if tray != nil {
				tray.Stop()
			}
		}},
	}

	err := winui.Run("CodeRoom 判题服务端", items, func(t winui.Tray) {
		tray = t
		t.SetTooltip(fmt.Sprintf("CodeRoom 判题服务端\n学生端连接地址 %s", addr))
		t.Notify("CodeRoom 判题服务端已启动",
			fmt.Sprintf("学生端连接 %s。双击托盘图标打开教师端，右键可显示运行窗口或退出。", addr))
		// The server stopping on its own (a port clash, say) has to take the
		// icon with it, or the teacher is left with a tray icon for nothing.
		go func() {
			<-ctx.Done()
			t.Stop()
		}()
	})
	if err != nil {
		// No icon: put the window back rather than leaving the teacher with a
		// server they cannot see or stop. The server is already running in the
		// goroutine above, so this still returns true - saying otherwise would
		// have the caller start a second one on the same port.
		logger.Printf("托盘图标创建失败，改为显示运行窗口：%v", err)
		winui.ShowConsole()
		if err := <-done; err != nil {
			logger.Fatalf("服务端异常退出: %v", err)
		}
		logger.Println("服务端已停止")
		return true
	}

	stop()
	if err := <-done; err != nil {
		logger.Printf("服务端异常退出: %v", err)
	}
	logger.Println("服务端已停止")
	return true
}

func printBanner(srv *server.Server, dataDir string) {
	cfg := srv.Config()
	fmt.Println()
	fmt.Println("  CodeRoom 判题服务端")
	fmt.Println("  ────────────────────────────────────────────────")
	fmt.Printf("  数据目录   %s\n", dataDir)
	fmt.Printf("  监听地址   %s\n", cfg.Addr)
	fmt.Printf("  判题并发   %d\n", cfg.Workers)

	// Surfaced at startup so a missing compiler is noticed before an exam
	// rather than when the first submission comes back as a system error.
	avail := srv.JudgeableLanguages()
	names := map[string]string{"cpp": "C++", "c": "C", "python": "Python"}
	var ok, missing []string
	for _, lang := range []string{"cpp", "c", "python"} {
		if avail[lang] {
			ok = append(ok, names[lang])
		} else {
			missing = append(missing, names[lang])
		}
	}
	fmt.Printf("  可判语言   %s\n", strings.Join(ok, " / "))
	if len(missing) > 0 {
		fmt.Printf("  ⚠ 不可判   %s —— 请在教师端「系统」中配置编译器路径\n",
			strings.Join(missing, " / "))
	}
	fmt.Println()
	fmt.Println("  教师端请在浏览器打开（含管理口令，请勿转发给学生）:")
	fmt.Printf("     %s\n", srv.AdminURL())
	fmt.Println()
	if ips := server.LocalIPs(); len(ips) > 0 {
		fmt.Println("  告知学生端的服务器地址:")
		for _, ip := range ips {
			fmt.Printf("     %s:%d\n", ip, srv.Port())
		}
	} else {
		fmt.Println("  未检测到局域网地址，学生端可能无法连接。")
	}
	fmt.Println()

	if !server.FirewallReady(srv.Port(), cfg.DiscoveryPort) {
		fmt.Println("  ⚠ " + server.FirewallHint(srv.Port(), cfg.DiscoveryPort))
		fmt.Println()
	}
	fmt.Printf("  管理口令   %s\n", cfg.AdminToken)
	fmt.Println("  ────────────────────────────────────────────────")
	fmt.Println("  按 Ctrl+C 停止")
	fmt.Println()
}

// defaultDataDir keeps data next to the executable so the whole thing stays a
// portable folder a teacher can copy to a USB stick.
func defaultDataDir() string {
	return filepath.Join(executableDir(), "coderoom-data")
}

func executableDir() string {
	exe, err := os.Executable()
	if err != nil {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
