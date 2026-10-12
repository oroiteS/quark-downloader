// feiwu 主程序装配：数据目录/设置/秘钥（store）→ webapi.App → gin
// server → rlimit 自提 → 监听 → 自动开浏览器 → ctx 取消优雅退出。
//
// 系列 3/5（store + Web API）的装配主干：run(ctx) 只做装配与生命周期，
// 应用态全部收敛在 webapi.App（后续 restoreSession/restoreTasks/引擎
// 接线都挂 App 内部方法位，main 不再为此改动——裁决 P1）。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"quark-downloader/internal/engine"
	"quark-downloader/internal/store"
	"quark-downloader/internal/webapi"
)

// shutdownTimeout 优雅关闭预算：HTTP 排空；Manager.Close（E6 保留断点，
// Task 9 已接线）与 store 落盘（TODO Task 11）共享同一预算。
const shutdownTimeout = 5 * time.Second

// runConfig run 的可注入点（测试用）；零值即生产路径。
type runConfig struct {
	onReady func(addr string) // 监听就绪回调（测试拿实际端口）
}

type runOption func(*runConfig)

func withOnReady(f func(addr string)) runOption {
	return func(c *runConfig) { c.onReady = f }
}

// run 核心装配与生命周期（main 可测试化的全部逻辑）：
//
//	store.ResolvePaths（G7）→ LoadSettings/SecretStore → webapi.New →
//	app.Start（restore 钩子，当前空位）→ rlimit 自提 → 监听（端口写
//	日志）→ 自动开浏览器（G8）→ <-ctx.Done() → srv.Shutdown（OnShutdown
//	先行关闭 SSE hub）→ app.Close → join serve goroutine（无泄漏）。
//
// 返回前 join serveDone：run 返回 ⇒ serve goroutine 已退出。
func run(ctx context.Context, opts ...runOption) error {
	var cfg runConfig
	for _, o := range opts {
		o(&cfg)
	}

	// 1) 数据目录与持久化装配（G7；目录创建是首次写入者职责，这里为
	// 秘钥/设置预建）。
	paths := store.ResolvePaths()
	if err := os.MkdirAll(paths.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s: %w", paths.DataDir, err)
	}
	settings := store.LoadSettings(paths) // 缺失/坏文件 → 默认，不挡启动
	secrets := store.NewSecretStore(paths)
	slog.Info("数据目录就绪", "dir", paths.DataDir)
	// Manager 装配（Task 9）：DestDir/Threads/MaxConcurrentTasks 与
	// settings 同源——webapi 的 TaskCtx.Dest 组装与预算分配
	// （AllocateBudget 的 cap/concurrency）因此与引擎内部一致。
	// onEvent 传 nil 是引擎文档化容忍（Task 12 接 hub.Broadcast）。
	mgr := engine.NewManager(engine.Config{
		Threads:            settings.Threads,
		Mode:               engine.ModeA, // settings 无 mode 字段，缺省直写
		MaxConcurrentTasks: settings.MaxConcurrentTasks,
		DestDir:            settings.DestDir,
	}, nil)

	app := webapi.New(webapi.Deps{
		Paths:    paths,
		Settings: settings,
		Secrets:  secrets,
		Manager:  mgr,
	})
	handler := app.Router()

	// 2) 启动钩子：restoreSession（Task 7）/restoreTasks（Task 11）——
	// 当前为 App 内空方法位，签名与调用链已冻结。
	if err := app.Start(ctx); err != nil {
		return fmt.Errorf("应用启动钩子: %w", err)
	}

	// 3) rlimit 自提（选型报告 §5.2：512 连发 + 每片文件句柄，macOS 默认
	// soft 256 不够）。失败不致命：记日志继续。
	before, after, err := raiseFDLimit()
	switch {
	case err != nil:
		slog.Warn("fd 上限自提失败（以当前上限继续）", "err", err)
	case after > before:
		slog.Info("fd 上限已自提", "before", before, "after", after)
	}

	// 4) 监听：默认 127.0.0.1:0（随机端口写日志；FEIWU_PORT 可配——设置
	// 无端口字段，端口仅 env 可配是设计取舍）。
	addr := listenAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", addr, err)
	}
	url := "http://" + ln.Addr().String()
	srv := &http.Server{Handler: handler}
	// Shutdown 开始即触发 App.Close（关 SSE hub 让长连接退出，排空不必
	// 拖满预算后强关）；幂等，下方关闭链再调一次无妨。
	srv.RegisterOnShutdown(app.Close)
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	slog.Info("feiwu 监听中", "url", url)
	if cfg.onReady != nil {
		cfg.onReady(url)
	}

	// 5) 自动开浏览器（G8）：失败仅记日志，绝不影响服务。
	if err := openBrowser(url); err != nil {
		slog.Warn("自动开浏览器失败", "err", err)
	}

	// 6) 信号驱动优雅关闭（SIGINT/SIGTERM → shutdown → app.Close）。
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close() // 排空超时：强关兜底
	}
	// App 生命周期收口（hub 已由 OnShutdown 关闭；此处幂等再收——
	// manager.Close 的 E6 保留断点语义在并发/重复调用下幂等安全）。
	// TODO(Task 11): store 落盘（tasks.json）经 App.Close 扩展。
	app.Close()
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	slog.Info("feiwu 已退出")
	return nil
}

// listenAddr 监听地址：FEIWU_PORT 可配；缺省 127.0.0.1:0（随机端口写日志）。
func listenAddr() string {
	if p := os.Getenv("FEIWU_PORT"); p != "" {
		return net.JoinHostPort("127.0.0.1", p)
	}
	return net.JoinHostPort("127.0.0.1", "0")
}

// openBrowser 自动开浏览器（G8）：FEIWU_NO_BROWSER=1 跳过（测试/无头
// 环境约定）；其余走平台打开。
func openBrowser(url string) error {
	if os.Getenv("FEIWU_NO_BROWSER") == "1" {
		return nil
	}
	return launchBrowser(url)
}

// launchBrowser 平台打开（标准库 os/exec，零新增依赖）。
// 变量形态便于测试注入——测试绝不真开浏览器。
var launchBrowser = func(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() // 异步开页，不等待浏览器进程
}

// main 只做信号接线与退出码；全部装配在 run（可测试化）。
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("feiwu 异常退出", "err", err)
		os.Exit(1)
	}
}
