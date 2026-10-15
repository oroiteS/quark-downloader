package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quark-downloader/internal/store"
	"quark-downloader/internal/webapi"
)

// TestRunLifecycle 端到端冒烟（Task 1 核心验收，Task 5 换真装配后语义
// 不降）：无浏览器 + 随机端口下 run 可启动；GET /api/health → 200
// {"status":"ok"}；ctx 取消后 run 干净返回（serve goroutine 已 join，
// 无泄漏）、监听端口真正下线。
func TestRunLifecycle(t *testing.T) {
	t.Setenv("FEIWU_NO_BROWSER", "1")
	t.Setenv("FEIWU_DATA_DIR", t.TempDir()) // 隔离数据目录（ResolvePaths 真实定位）

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, withOnReady(func(addr string) { ready <- addr })) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run 在就绪前退出: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run 启动超时（未就绪）")
	}

	resp, err := http.Get(addr + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/api/health 状态码 = %d, 期望 200", resp.StatusCode)
	}
	// gin 的 JSON Content-Type 带 charset 后缀。
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, 期望 application/json*", ct)
	}
	if want := `{"status":"ok"}`; string(body) != want {
		t.Fatalf("/api/health 响应体 = %q, 期望 %q", body, want)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消后 run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 run 未退出（serve goroutine 泄漏或挂起）")
	}

	// run 返回后监听端口必须下线：新连接（禁用 keep-alive 复用）被拒。
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(addr + "/api/health"); err == nil {
		resp.Body.Close()
		t.Fatal("run 返回后端口仍可访问（server 未真正关闭）")
	}
}

// TestRouterHealth run 所用 router（webapi.App）的 httptest 级冒烟：
// /api/health 端点在真 router 上可用（裁决 1：Router() 是 App 门面）。
func TestRouterHealth(t *testing.T) {
	srv := httptest.NewServer(webapi.New(webapi.Deps{}).Router())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应体: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if want := `{"status":"ok"}`; string(body) != want {
		t.Fatalf("响应体 = %q, 期望 %q", body, want)
	}
}

// TestResolvePathsSmoke run 的数据目录现由 store.ResolvePaths 定位
// （G7；完整矩阵测试在 internal/store）。这里冒烟 FEIWU_DATA_DIR 覆盖
// 一切——run 的测试隔离依赖该语义。
func TestResolvePathsSmoke(t *testing.T) {
	t.Run("FEIWU_DATA_DIR 覆盖一切", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("FEIWU_DATA_DIR", dir)
		p := store.ResolvePaths()
		if p.DataDir != dir {
			t.Fatalf("DataDir = %q, 期望 %q", p.DataDir, dir)
		}
		if p.Root != dir {
			t.Fatalf("Root = %q, 期望与 DataDir 同值", p.Root)
		}
	})
	t.Run("设置/任务文件落 DataDir", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("FEIWU_DATA_DIR", dir)
		p := store.ResolvePaths()
		if got, want := p.SettingsFile, dir+"/settings.json"; got != want {
			t.Fatalf("SettingsFile = %q, 期望 %q", got, want)
		}
		if got, want := p.TasksFile, dir+"/tasks.json"; got != want {
			t.Fatalf("TasksFile = %q, 期望 %q", got, want)
		}
	})
}

// TestListenAddr 监听地址：FEIWU_PORT 可配，缺省随机端口（127.0.0.1:0）。
func TestListenAddr(t *testing.T) {
	t.Run("FEIWU_PORT 指定端口", func(t *testing.T) {
		t.Setenv("FEIWU_PORT", "18080")
		if got, want := listenAddr(), "127.0.0.1:18080"; got != want {
			t.Fatalf("listenAddr() = %q, 期望 %q", got, want)
		}
	})
	t.Run("缺省随机端口", func(t *testing.T) {
		t.Setenv("FEIWU_PORT", "")
		if got, want := listenAddr(), "127.0.0.1:0"; got != want {
			t.Fatalf("listenAddr() = %q, 期望 %q", got, want)
		}
	})
}

// TestRunWithWindowRunner 注入窗口运行器：收到完整 server URL、阻塞
// 返回后 run 正常收尾（Task 1 窗口注入点验收）。
func TestRunWithWindowRunner(t *testing.T) {
	t.Setenv("FEIWU_NO_BROWSER", "1")       // 实现若误走 openBrowser 分支也不得真开浏览器
	t.Setenv("FEIWU_DATA_DIR", t.TempDir()) // 隔离数据目录（ResolvePaths 真实定位）

	got := make(chan string, 1)
	opt := withWindowRunner(func(serverURL string) error {
		got <- serverURL
		return nil
	})

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, opt, withOnReady(func(addr string) { ready <- addr }))
	}()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run 在就绪前退出: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run 启动超时（未就绪）")
	}

	// 断言：runner 收到 "http://127.0.0.1:<port>" 完整 URL（与 onReady
	// 收到的同一 URL——run 两者传同值）。
	select {
	case serverURL := <-got:
		if serverURL != addr {
			t.Fatalf("windowRunner 收到 %q, 与 onReady 的 %q 不一致", serverURL, addr)
		}
		if !strings.HasPrefix(serverURL, "http://127.0.0.1:") {
			t.Fatalf("windowRunner 收到 %q, 期望 http://127.0.0.1:<port> 形式", serverURL)
		}
	case err := <-done:
		t.Fatalf("windowRunner 调用前 run 已退出: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("windowRunner 未被调用（监听成功后未触发窗口）")
	}

	// Task 7 起 runner 返回 nil 即触发 run 内 cancel 自行收口（下方
	// TestRunWindowRunnerCloseTriggersShutdown 专测）；此处 cancel() 对
	// 外部 signal ctx 的取消此时已是 no-op，select done 仅兜底。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("窗口关闭后 run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("窗口关闭后 run 未退出（serve goroutine 泄漏或挂起）")
	}
}

// TestRunNoWindowEnvSuppressesRunner FEIWU_NO_WINDOW=1：设置了
// windowRunner 也不得调用（headless 不崩），且退回旧的自动开浏览器
// 行为——无窗口模式下行为与现状完全一致。
func TestRunNoWindowEnvSuppressesRunner(t *testing.T) {
	t.Setenv("FEIWU_NO_WINDOW", "1")
	t.Setenv("FEIWU_DATA_DIR", t.TempDir())
	orig := launchBrowser
	defer func() { launchBrowser = orig }()
	browserGot := make(chan string, 1)
	launchBrowser = func(u string) error { browserGot <- u; return nil }

	got := make(chan string, 1)
	opt := withWindowRunner(func(serverURL string) error {
		got <- serverURL
		return nil
	})

	ready := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, opt, withOnReady(func(addr string) { ready <- addr }))
	}()

	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("run 在就绪前退出: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run 启动超时（未就绪）")
	}

	// ctx 取消（no-window 下 runner 非阻塞，run 正常走到信号等待）。
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消后 run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 run 未退出")
	}

	// run 返回后再断言：runner 从未被调用；openBrowser 分支照旧执行。
	select {
	case serverURL := <-got:
		t.Fatalf("FEIWU_NO_WINDOW=1 时 windowRunner 仍被调用, serverURL=%q", serverURL)
	default:
	}
	select {
	case u := <-browserGot:
		if !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Fatalf("无窗口回退 openBrowser 收到 %q, 期望 http://127.0.0.1:<port> 形式", u)
		}
	case <-time.After(time.Second):
		t.Fatal("FEIWU_NO_WINDOW=1 时未回退 openBrowser（旧行为丢失）")
	}
}

// TestRunWindowRunnerCloseTriggersShutdown Task 7 关窗退出收口：窗口
// runner 返回 nil（窗口关闭）→ run 不得滞留在信号等待（旧缺陷：窗口关
// 了进程不退），须直接走收尾链（srv.Shutdown → app.Close → join serve）
// 并返回 nil。外部 signal ctx 全程未取消——退出动力只能来自 run 内的
// 关窗 cancel。run 返回本身即收尾完成的证据（返回前 join serveDone），
// 端口下线再证一层。
func TestRunWindowRunnerCloseTriggersShutdown(t *testing.T) {
	t.Setenv("FEIWU_NO_BROWSER", "1")
	t.Setenv("FEIWU_DATA_DIR", t.TempDir())

	ready := make(chan string, 1)
	opt := withWindowRunner(func(string) error { return nil })

	ctx, cancel := context.WithCancel(context.Background()) // 外部 ctx 保持未取消
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, opt, withOnReady(func(addr string) { ready <- addr })) }()

	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("run 在就绪前退出: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("run 启动超时（未就绪）")
	}

	// runner 返回 nil → run 自行收口（不依赖外部 ctx 取消）。
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("窗口关闭后 run 返回错误: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("窗口关闭后 run 未返回（关窗不退出的旧缺陷仍在）")
	}

	// 收尾已执行的额外证据：监听端口真正下线（禁用 keep-alive 复用）。
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(addr + "/api/health"); err == nil {
		resp.Body.Close()
		t.Fatal("run 返回后端口仍可访问（收尾链未执行）")
	}
}

// TestRunWindowRunnerErrorWrapped 降级红线（Review Focus ④，fakeRunner
// 层）：窗口运行器返回 error → run 以「窗口启动失败: %w」语义包装上抛
// （errors.Is 穿透成立），且**立即返回**——绝不静默吞掉后无界面挂着。
// 平台无关（不触 wails），CI ubuntu 亦可执行。
func TestRunWindowRunnerErrorWrapped(t *testing.T) {
	t.Setenv("FEIWU_NO_BROWSER", "1")
	t.Setenv("FEIWU_DATA_DIR", t.TempDir())

	sentinel := errors.New("wails 假想失败")
	opt := withWindowRunner(func(string) error { return sentinel })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, opt) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("windowRunner 返回 error 但 run 返回 nil（静默吞错，无界面挂着）")
		}
		if !strings.Contains(err.Error(), "窗口启动失败") {
			t.Fatalf("run 错误 = %q, 期望包含「窗口启动失败」包装", err)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("run 错误 = %v, 期望以 %%w 包装 sentinel（errors.Is 穿透）", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("windowRunner 失败后 run 未返回（进程活着但无界面）")
	}
}

// TestOpenBrowser 自动开浏览器（G8）：FEIWU_NO_BROWSER=1 跳过；默认走
// 平台打开（launchBrowser 可注入，测试绝不真开浏览器）。
func TestOpenBrowser(t *testing.T) {
	orig := launchBrowser
	defer func() { launchBrowser = orig }()

	t.Run("NO_BROWSER 跳过", func(t *testing.T) {
		t.Setenv("FEIWU_NO_BROWSER", "1")
		called := false
		launchBrowser = func(string) error { called = true; return nil }
		if err := openBrowser("http://127.0.0.1:1"); err != nil {
			t.Fatalf("openBrowser: %v", err)
		}
		if called {
			t.Fatal("FEIWU_NO_BROWSER=1 时仍调用了平台打开")
		}
	})
	t.Run("默认调用平台打开", func(t *testing.T) {
		t.Setenv("FEIWU_NO_BROWSER", "")
		got := ""
		launchBrowser = func(u string) error { got = u; return nil }
		if err := openBrowser("http://127.0.0.1:1"); err != nil {
			t.Fatalf("openBrowser: %v", err)
		}
		if got != "http://127.0.0.1:1" {
			t.Fatalf("launchBrowser 收到 %q, 期望原样透传 URL", got)
		}
	})
}
