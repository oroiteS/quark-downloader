//go:build darwin || windows

// Wails 窗口运行器装配面测试（Task 2）。wails.Run 本体不可 headless
// 测试（无显示器环境直接失败），只测装配面：
//
//  1. buildAppOptions 的窗口参数（计划冻结值：标题「飞舞」，1280×832，
//     最小 960×600，StartHidden: false）；
//  2. URL 直传语义——首跳 HTML 导航页中的目标 URL 与 server 完整 URL 逐字
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
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
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

// TestBuildAppOptionsURLDirect URL 直传（不拼路径）：首跳 handler 返回的
// 必须是一页 200 的 HTML 导航页（Content-Type text/html），body 中的目标
// URL 与 serverURL 逐字一致。不走 HTTP 302——darwin 资产响应经
// WKURLSchemeHandler 投递，不解析 HTTP 重定向语义，真机实证 302 body
// 「Found.」被当普通内容渲染成白屏（机制见 window.go bootRedirectHandler）。
// URL 多一个 '/' 都会让 SSE/静态资源走上歧路。首跳经 buildAppOptions
// 产物的 AssetServer.Handler 验证（httptest 层）。
func TestBuildAppOptionsURLDirect(t *testing.T) {
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 防跟随：直接断言首跳页本体
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
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatalf("读首跳 body: %v", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("首跳状态码 = %d, 期望 200（HTML 导航页，非 HTTP 重定向）", resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("首跳 Content-Type = %q, 期望 text/html", ct)
		}
		if !strings.Contains(string(body), serverURL) {
			t.Fatalf("首跳页 body 未含目标 URL %q, body: %s", serverURL, body)
		}
		if strings.Contains(string(body), serverURL+"/") {
			t.Fatalf("首跳页 URL 被拼了路径, 期望逐字 %q, body: %s", serverURL, body)
		}
	}
}

// TestCloseConfirmDialogOptionsDefaultButton 关窗确认弹框参数装配
// （评审第 1 项，平台 tag 内）：冻结值不动（Title/Message/Buttons/
// CancelButton），DefaultButton 按本平台断言——保守策略「回车≠退出」：
// darwin 期望 "取消"（回车=取消=留窗）；windows 期望小写 "no"
// （wails 仅在 ToLower(DefaultButton)=="no" 时设 MB_DEFBUTTON2，
// 回车=No=不退出；此前固定 "取消" 在 Windows 静默不生效）。
// 全平台映射表见 window_logic_test.go 的 TestDefaultCloseConfirmButton。
func TestCloseConfirmDialogOptionsDefaultButton(t *testing.T) {
	opts := closeConfirmDialogOptions()
	if opts.Title != "飞舞" {
		t.Fatalf("Title = %q, 期望 %q", opts.Title, "飞舞")
	}
	if opts.Message != "您还有任务未完成，确定关闭软件吗？" {
		t.Fatalf("Message = %q, 期望计划冻结文案", opts.Message)
	}
	if len(opts.Buttons) != 2 || opts.Buttons[0] != "退出" || opts.Buttons[1] != "取消" {
		t.Fatalf("Buttons = %v, 期望 [退出 取消]", opts.Buttons)
	}
	if opts.CancelButton != "取消" {
		t.Fatalf("CancelButton = %q, 期望 %q", opts.CancelButton, "取消")
	}
	var want string
	switch runtime.GOOS {
	case "windows":
		want = "no"
	default:
		want = "取消"
	}
	if opts.DefaultButton != want {
		t.Fatalf("DefaultButton = %q, 期望 %q（GOOS=%s，保守策略回车≠退出）",
			opts.DefaultButton, want, runtime.GOOS)
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

// TestBuildAppOptionsOnBeforeCloseWiring OnBeforeClose 接线（评审第 3 项
// 下沉后的剩余平台面）：三分支判定的核心断言已迁平台无关
// （window_logic_test.go 的 TestShouldPreventClose，CI ubuntu 可编译执行），
// 此处只守装配面——OnBeforeClose 已接线、hasActiveTasks/confirmClose
// 原样注入 shouldPreventClose、OnBeforeClose 收到的 ctx 透传给
// confirmClose（真实弹框 ctx 消费者，透传断裂会让弹框失去窗口归属）。
func TestBuildAppOptionsOnBeforeCloseWiring(t *testing.T) {
	origConfirm := confirmClose
	defer func() { confirmClose = origConfirm }()

	type wiringCtxKey struct{}
	called := false
	var gotCtx context.Context
	confirmClose = func(ctx context.Context) bool { called = true; gotCtx = ctx; return false }
	opt := buildAppOptions("http://127.0.0.1:45678", func() bool { return true })
	if opt.OnBeforeClose == nil {
		t.Fatal("OnBeforeClose 未接线（关窗确认缺失）")
	}
	ctx := context.WithValue(context.Background(), wiringCtxKey{}, "probe")
	if prevent := opt.OnBeforeClose(ctx); !prevent {
		t.Fatal("有活动任务 + 用户取消 OnBeforeClose = false, 期望 true（取消应阻止关闭）")
	}
	if !called {
		t.Fatal("有活动任务未调用确认函数（confirmClose 注入断裂）")
	}
	if gotCtx != ctx {
		t.Fatal("confirmClose 未收到 OnBeforeClose 的 ctx（透传断裂）")
	}
}

// TestNewWailsRunnerEmptyURL 空 serverURL 校验：runner 以空串调用直接
// 返回 error——绝不进入 wails.Run（本测试空串短路，安全不开窗），更不得
// 让首跳导航页在无目标地址时产出。
func TestNewWailsRunnerEmptyURL(t *testing.T) {
	r := newWailsRunner(nil, nil)
	if r == nil {
		t.Fatal("newWailsRunner = nil")
	}
	if err := r(""); err == nil {
		t.Fatal("空 serverURL 未返回 error（相对 Location 风险）")
	}
}
