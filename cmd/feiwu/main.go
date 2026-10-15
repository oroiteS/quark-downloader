// feiwu 主程序装配：数据目录/设置/秘钥（store）→ webapi.App → gin
// server → rlimit 自提 → 监听 → Wails 原生窗口（默认，无浏览器）→
// ctx 取消优雅退出；无窗口模式（FEIWU_NO_WINDOW=1 / 非桌面）纯服务端，
// 仅 FEIWU_OPEN_BROWSER=1 时可选开浏览器（Task 3 行为矩阵）。
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
	"sync/atomic"
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
	// onApp webapi.App 装配回调（Task 7）：run 在 webapi.New + SetManager
	// 后调用（与 onReady 对称的注入点）。窗口模式的 HasActiveTasks 需要
	// 跨函数传递——runner 装配在 main，而 app 建于 run 内，main 侧闭包
	// 经此回调拿 app 延迟解析（跨 goroutine 读靠 atomic 容器）。
	onApp func(*webapi.App)
	// windowRunner 窗口运行器（Task 2 wails 注入）：server 监听成功后
	// 被调用，阻塞直到窗口关闭；返回 error 视为窗口失败。nil（非桌面
	// 平台）→ 无窗口模式：纯服务端，浏览器仅按 FEIWU_OPEN_BROWSER
	// 可选开启（Task 3）。FEIWU_NO_WINDOW=1 优先级高于它。
	windowRunner func(serverURL string) error
}

type runOption func(*runConfig)

func withOnReady(f func(addr string)) runOption {
	return func(c *runConfig) { c.onReady = f }
}

func withOnApp(f func(*webapi.App)) runOption {
	return func(c *runConfig) { c.onApp = f }
}

func withWindowRunner(f func(serverURL string) error) runOption {
	return func(c *runConfig) { c.windowRunner = f }
}

// run 核心装配与生命周期（main 可测试化的全部逻辑）：
//
//	store.ResolvePaths（G7）→ LoadSettings/SecretStore → webapi.New →
//	app.Start（restore 钩子，当前空位）→ rlimit 自提 → 监听（端口写
//	日志）→ 窗口/浏览器分支（窗口为主路径；无窗口模式为纯服务端，
//	仅 FEIWU_OPEN_BROWSER=1 时开浏览器，Task 3）→ <-ctx.Done() →
//	srv.Shutdown（OnShutdown 先行关闭 SSE hub）→ app.Close → join
//	serve goroutine（无泄漏）。
//
// 返回前 join serveDone：run 返回 ⇒ serve goroutine 已退出。
func run(ctx context.Context, opts ...runOption) error {
	var cfg runConfig
	for _, o := range opts {
		o(&cfg)
	}

	// Task 7 关窗退出收口：窗口 runner 返回（窗口关闭）即 cancel 内层
	// ctx，走与信号一致的收尾链（srv.Shutdown → app.Close → join serve）
	// 后返回——旧实现窗口关闭后进程滞留在 <-ctx.Done() 不退。defer 保证
	// 各 error 路径也无 ctx 泄漏；FEIWU_NO_WINDOW=1 / 无 runner 路径不触
	// 及此 cancel，行为与现状完全一致。
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// 1) 数据目录与持久化装配（G7；目录创建是首次写入者职责，这里为
	// 秘钥/设置预建）。
	paths := store.ResolvePaths()
	if err := os.MkdirAll(paths.DataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s: %w", paths.DataDir, err)
	}
	settings := store.LoadSettings(paths) // 缺失/坏文件 → 默认，不挡启动
	secrets := store.NewSecretStore(paths)
	slog.Info("数据目录就绪", "dir", paths.DataDir)

	app := webapi.New(webapi.Deps{
		Paths:    paths,
		Settings: settings,
		Secrets:  secrets,
	})
	// Manager 装配（Task 12，P4 Ruling）：先建 App（hub 就绪）→
	// NewManager(cfg, app.EngineEvent)（引擎事件直连 hub 广播，500ms
	// 节流在引擎层）→ SetManager（启动前一次性注入，无并发面）。
	// DestDir/Threads/MaxConcurrentTasks 与 settings 同源——webapi 的
	// TaskCtx.Dest 组装与预算分配（AllocateBudget 的 cap/concurrency）
	// 因此与引擎内部一致。
	mgr := engine.NewManager(engine.Config{
		Threads:            settings.Threads,
		Mode:               engine.ModeA, // settings 无 mode 字段，缺省直写
		MaxConcurrentTasks: settings.MaxConcurrentTasks,
		DestDir:            settings.DestDir,
	}, app.EngineEvent)
	app.SetManager(mgr)
	if cfg.onApp != nil {
		cfg.onApp(app)
	}
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

	// 5) 窗口/浏览器分支（Task 3 行为矩阵）：窗口为主路径——注入了窗口
	// 运行器且未设 FEIWU_NO_WINDOW → 阻塞运行窗口直到关闭；返回 error
	// 视为窗口失败，包装上抛、绝不 fallback 浏览器。无窗口模式
	// （FEIWU_NO_WINDOW=1 或 runner 为 nil）为纯服务端：仅
	// FEIWU_OPEN_BROWSER=1 显式选择加入时才 openBrowser——未设/非 "1"
	// 即不调，静默无 warn（不开浏览器是明确意图而非失败）。窗口正常
	// 关闭（runner 返回 nil）→ cancel 触发收尾链：关窗即退出（Task 7）。
	if cfg.windowRunner != nil && os.Getenv("FEIWU_NO_WINDOW") != "1" {
		if err := cfg.windowRunner(url); err != nil {
			return fmt.Errorf("窗口启动失败: %w", err)
		}
		cancel()
	} else if os.Getenv("FEIWU_OPEN_BROWSER") == "1" {
		if err := openBrowser(url); err != nil {
			slog.Warn("自动开浏览器失败", "err", err)
		}
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

// openBrowser 无窗口模式可选的浏览器打开（G8 起家，Task 3 起调用时机
// 由 run 的行为矩阵门控：仅无窗口模式且 FEIWU_OPEN_BROWSER=1）。
// FEIWU_NO_BROWSER=1 跳过（已废弃开关，保留兼容，仅无窗口模式生效）；
// 其余走平台打开。
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
	// 窗口装配（Task 2）：默认 Wails 原生窗口——直接打开见窗口，不跳
	// 浏览器；server 监听成功后才开窗（先监听、拿到真实地址、再开窗口，
	// 消除「窗口打开但 404」竞态）。FEIWU_NO_WINDOW=1 或非 darwin/windows
	// （runner 为 nil）→ 无窗口模式：纯服务端，仅 FEIWU_OPEN_BROWSER=1
	// 时 openBrowser（Task 3 行为矩阵）；测试一律注入 fakeRunner，绝不
	// 真开窗口。
	//
	// Task 7：signal ctx 直通 newWailsRunner（SIGINT/SIGTERM → runtime.Quit
	// 关窗口 → run 收尾链）；HasActiveTasks 跨函数延迟解析——app 建于 run
	// 内（onApp 回调交给 main 侧 atomic 容器），而 OnBeforeClose 在 wails
	// 事件循环 goroutine 执行，atomic.Pointer 保证跨 goroutine 可见性。
	// 时序上 runner 晚于 onApp 被调用（监听成功后才开窗），Load 必非 nil，
	// nil 分支仅防御。
	var appRef atomic.Pointer[webapi.App]
	if err := run(ctx,
		withOnApp(func(a *webapi.App) { appRef.Store(a) }),
		withWindowRunner(newWailsRunner(ctx, func() bool {
			if a := appRef.Load(); a != nil {
				return a.HasActiveTasks()
			}
			return false
		})),
	); err != nil {
		slog.Error("feiwu 异常退出", "err", err)
		os.Exit(1)
	}
}
