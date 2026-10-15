//go:build darwin || windows

// Wails 窗口运行器装配面测试（Task 2）。wails.Run 本体不可 headless
// 测试（无显示器环境直接失败），只测装配面：
//
//  1. buildAppOptions 的窗口参数（计划冻结值：标题「飞舞」，1280×832，
//     最小 960×600，StartHidden: false）；
//  2. URL 直传语义——窗口首跳 302 的 Location 与 server 完整 URL 逐字
//     一致，不拼任何路径/锚点（SSE /api/events 依赖真实 HTTP 端口，
//     不走 assetserver 接管路由；首跳机制见 window.go 文件头偏差记录）；
//  3. newWailsRunner 装配面存在且非 nil（注入 run 的 closure 形态）。
//
// wails.Run 的 error 透传包装语义（「窗口启动失败: %w」）发生在 run()
// 的 fakeRunner 层，平台无关，见 main_test.go 的
// TestRunWindowRunnerErrorWrapped。
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBuildAppOptionsWindowParams 计划冻结的窗口参数逐字核对：
// 标题「飞舞」，1280×832，最小 960×600，StartHidden: false。
func TestBuildAppOptionsWindowParams(t *testing.T) {
	opt := buildAppOptions("http://127.0.0.1:45678", nil)
	if opt.Title != "飞舞" {
		t.Fatalf("Title = %q, 期望 %q", opt.Title, "飞舞")
	}
	if opt.Width != 1280 || opt.Height != 832 {
		t.Fatalf("初始尺寸 = %dx%d, 期望 1280x832", opt.Width, opt.Height)
	}
	if opt.MinWidth != 960 || opt.MinHeight != 600 {
		t.Fatalf("最小尺寸 = %dx%d, 期望 960x600", opt.MinWidth, opt.MinHeight)
	}
	if opt.StartHidden {
		t.Fatal("StartHidden = true, 期望 false（启动即见窗口）")
	}
}

// TestBuildAppOptionsURLDirect URL 直传（不拼路径）：窗口首跳 302 的
// Location 必须与 server 完整 URL 逐字一致——多一个 '/' 都会让 SSE/
// 静态资源走上歧路，也偏离「窗口直连 http://127.0.0.1:<port>」的约束。
// 首跳经 buildAppOptions 产物的 AssetServer.Handler 验证（httptest 层）。
func TestBuildAppOptionsURLDirect(t *testing.T) {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随，取 302 本体断言
		},
	}
	for _, serverURL := range []string{
		"http://127.0.0.1:45678", // 默认端口形态（Task 6 后）
		"http://127.0.0.1:0",     // 理论不出现，验证不做改写
	} {
		srv := httptest.NewServer(buildAppOptions(serverURL, nil).AssetServer.Handler)
		resp, err := client.Get(srv.URL + "/")
		srv.Close()
		if err != nil {
			t.Fatalf("GET 首跳 handler: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("首跳状态码 = %d, 期望 302", resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != serverURL {
			t.Fatalf("首跳 Location = %q, 期望原样直传 %q（不拼路径）", loc, serverURL)
		}
	}
}

// TestNewWailsRunnerAssembly 装配面存在性：newWailsRunner 返回非 nil
// 的 func(string) error（main 装配处 withWindowRunner(newWailsRunner(ctx,
// hasActiveTasks)) 的消费形态）；不调用它——调用即 wails.Run 真开窗口。
func TestNewWailsRunnerAssembly(t *testing.T) {
	r := newWailsRunner(nil, nil)
	if r == nil {
		t.Fatal("newWailsRunner() = nil, 期望非 nil 窗口运行器（darwin/windows 桌面构建）")
	}
}

// TestBuildAppOptionsOnBeforeClose 关窗确认三分支（Task 7；真弹框不可
// headless 测试，confirmClose 包级变量注入替换——真弹框人工验收归 Task 4）：
// ①无活动任务 → 返回 false 放行且确认函数未被调用；
// ②有活动任务 + confirmClose 返回 false（用户取消）→ 返回 true 阻止（窗口保留）；
// ③有活动任务 + confirmClose 返回 true（用户确认退出）→ 返回 false 放行（进程退出）。
func TestBuildAppOptionsOnBeforeClose(t *testing.T) {
	origConfirm := confirmClose
	defer func() { confirmClose = origConfirm }()
	inactive := func() bool { return false }
	active := func() bool { return true }

	t.Run("无活动任务放行且不弹框", func(t *testing.T) {
		called := false
		confirmClose = func(context.Context) bool { called = true; return true }
		opt := buildAppOptions("http://127.0.0.1:45678", inactive)
		if opt.OnBeforeClose == nil {
			t.Fatal("OnBeforeClose 未接线（关窗确认缺失）")
		}
		if prevent := opt.OnBeforeClose(context.Background()); prevent {
			t.Fatal("无活动任务 OnBeforeClose = true, 期望 false（直接放行关闭）")
		}
		if called {
			t.Fatal("无活动任务仍调用了确认函数（应短路放行）")
		}
	})
	t.Run("用户取消阻止关闭", func(t *testing.T) {
		confirmClose = func(context.Context) bool { return false }
		opt := buildAppOptions("http://127.0.0.1:45678", active)
		if prevent := opt.OnBeforeClose(context.Background()); !prevent {
			t.Fatal("用户取消 OnBeforeClose = false, 期望 true（窗口保留）")
		}
	})
	t.Run("用户确认放行退出", func(t *testing.T) {
		confirmClose = func(context.Context) bool { return true }
		opt := buildAppOptions("http://127.0.0.1:45678", active)
		if prevent := opt.OnBeforeClose(context.Background()); prevent {
			t.Fatal("用户确认 OnBeforeClose = true, 期望 false（进程退出）")
		}
	})
	t.Run("hasActiveTasks 为 nil 防御放行", func(t *testing.T) {
		called := false
		confirmClose = func(context.Context) bool { called = true; return true }
		opt := buildAppOptions("http://127.0.0.1:45678", nil)
		if prevent := opt.OnBeforeClose(context.Background()); prevent {
			t.Fatal("hasActiveTasks == nil OnBeforeClose = true, 期望 false（防御短路放行关闭）")
		}
		if called {
			t.Fatal("hasActiveTasks == nil 仍调用了确认函数（防御短路缺失）")
		}
	})
}

// TestNewWailsRunnerEmptyURL 空 serverURL 校验：runner 以空串调用直接
// 返回 error——绝不进入 wails.Run（本测试空串短路，安全不开窗），更不得
// 让首跳 302 产生相对 Location。
func TestNewWailsRunnerEmptyURL(t *testing.T) {
	r := newWailsRunner(nil, nil)
	if r == nil {
		t.Fatal("newWailsRunner = nil")
	}
	if err := r(""); err == nil {
		t.Fatal("空 serverURL 未返回 error（相对 Location 风险）")
	}
}
