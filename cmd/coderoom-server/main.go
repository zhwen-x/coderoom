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
)

func main() {
	defaultData := defaultDataDir()

	addr := flag.String("addr", "", "监听地址，如 :8080（留空使用配置文件中的值）")
	dataDir := flag.String("data", defaultData, "数据目录（数据库、测试数据、学生代码）")
	workers := flag.Int("workers", 0, "判题并发数（留空使用配置文件中的值）")
	openUI := flag.Bool("open", true, "启动后自动在浏览器中打开教师端")
	setupFirewall := flag.Bool("setup-firewall", false,
		"添加 Windows 防火墙入站规则后退出（需以管理员身份运行）")
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
		fmt.Printf("\n  已添加防火墙入站规则：TCP %d、UDP %d\n", srv.Port(), cfg.DiscoveryPort)
		fmt.Println("  现在学生端应该可以连上了。")
		return
	}

	printBanner(srv, *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *openUI {
		go openBrowser(srv.AdminURL())
	}

	if err := srv.Run(ctx); err != nil {
		logger.Fatalf("服务端异常退出: %v", err)
	}
	logger.Println("服务端已停止")
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
