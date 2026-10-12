// feiwu 主程序装配（Task 1 骨架）：数据目录 → 设置/Manager 占位 →
// HTTP 服务 → rlimit 自提 → 监听 → 自动开浏览器 → ctx 取消优雅退出。
//
// 系列 3/5（store + Web API）的装配骨架：store.Paths（Task 2）与
// webapi.New（Task 5）落地前，数据目录与 router 均为最小占位（见各
// TODO）。本任务核心验收是 run(ctx) 生命周期：启动 → 监听 127.0.0.1:0
// （端口写日志）→ ctx 取消后 server 优雅退出、serve goroutine 全部回收。
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
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// shutdownTimeout 优雅关闭预算：HTTP 排空；Task 5+ 接线后 Manager.Close
// （E6 保留断点）与 store 落盘共享同一预算。
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
//	数据目录定位（TODO Task 2 store.ResolvePaths）→ 设置/Manager 占位 →
//	占位 router（TODO Task 5 webapi.New）→ rlimit 自提 → 监听（端口写
//	日志）→ 自动开浏览器（G8）→ <-ctx.Done() → srv.Shutdown → join
//	serve goroutine（无泄漏）。
//
// 返回前 join serveDone：run 返回 ⇒ serve goroutine 已退出。
func run(ctx context.Context, opts ...runOption) error {
	var cfg runConfig
	for _, o := range opts {
		o(&cfg)
	}

	// 1) 数据目录（临时实现）。
	dataDir := resolveDataDir()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("创建数据目录 %s: %w", dataDir, err)
	}
	slog.Info("数据目录就绪", "dir", dataDir)
	// TODO(Task 2): store.ResolvePaths 回填——配置/下载目录与断点 DB 的规范路径。
	// TODO(Task 3): 设置加载（store.LoadSettings）。
	// TODO(Task 5+): 会话/Manager 装配（engine.NewManager + onEvent 接线）。

	handler := newRouter()

	// 2) rlimit 自提（选型报告 §5.2：512 连发 + 每片文件句柄，macOS 默认
	// soft 256 不够）。失败不致命：记日志继续。
	before, after, err := raiseFDLimit()
	switch {
	case err != nil:
		slog.Warn("fd 上限自提失败（以当前上限继续）", "err", err)
	case after > before:
		slog.Info("fd 上限已自提", "before", before, "after", after)
	}

	// 3) 监听：默认 127.0.0.1:0（随机端口写日志；FEIWU_PORT 可配）。
	addr := listenAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s: %w", addr, err)
	}
	url := "http://" + ln.Addr().String()
	srv := &http.Server{Handler: handler}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(ln) }()
	slog.Info("feiwu 监听中", "url", url)
	if cfg.onReady != nil {
		cfg.onReady(url)
	}

	// 4) 自动开浏览器（G8）：失败仅记日志，绝不影响服务。
	if err := openBrowser(url); err != nil {
		slog.Warn("自动开浏览器失败", "err", err)
	}

	// 5) 信号驱动优雅关闭（SIGINT/SIGTERM → shutdown → Manager.Close →
	// store 落盘；后两者 Task 5+ 接线）。
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close() // 排空超时：强关兜底
	}
	// TODO(Task 5+): Manager.Close（E6 保留断点）与 store 落盘。
	if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http serve: %w", err)
	}
	slog.Info("feiwu 已退出")
	return nil
}

// resolveDataDir 数据目录临时实现：FEIWU_DATA_DIR 优先，缺省 os.TempDir
// 下 feiwu 子目录。
// TODO(Task 2): store.ResolvePaths 回填。
func resolveDataDir() string {
	if d := os.Getenv("FEIWU_DATA_DIR"); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "feiwu")
}

// listenAddr 监听地址：FEIWU_PORT 可配；缺省 127.0.0.1:0（随机端口写日志）。
// TODO(Task 3): 设置可配端口（store.Settings 覆盖 env）。
func listenAddr() string {
	if p := os.Getenv("FEIWU_PORT"); p != "" {
		return net.JoinHostPort("127.0.0.1", p)
	}
	return net.JoinHostPort("127.0.0.1", "0")
}

// newRouter 最小占位 router：GET /api/health → 200 {"status":"ok"}。
// TODO(Task 5): webapi.New 回填——设置/会话/任务完整 API。
func newRouter() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
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
