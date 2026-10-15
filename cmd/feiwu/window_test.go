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
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBuildAppOptionsWindowParams 计划冻结的窗口参数逐字核对：
// 标题「飞舞」，1280×832，最小 960×600，StartHidden: false。
func TestBuildAppOptionsWindowParams(t *testing.T) {
	opt := buildAppOptions("http://127.0.0.1:45678")
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
		srv := httptest.NewServer(buildAppOptions(serverURL).AssetServer.Handler)
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
// 的 func(string) error（main 装配处 withWindowRunner(newWailsRunner())
// 的消费形态）；不调用它——调用即 wails.Run 真开窗口。
func TestNewWailsRunnerAssembly(t *testing.T) {
	r := newWailsRunner()
	if r == nil {
		t.Fatal("newWailsRunner() = nil, 期望非 nil 窗口运行器（darwin/windows 桌面构建）")
	}
}
