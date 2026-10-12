package main

import (
	"context"
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
