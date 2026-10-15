//go:build darwin || windows

// Wails 原生窗口运行器（系列 4/5 Task 2）：Wails 仅作窗口壳，窗口直连
// 本地 server 完整 URL——gin + 随机端口架构不动。wails.Run 的 ObjC
// bootstrap 让进程拥有完整 NSApplication 生命周期，是 macOS Dock 弹跳
// 的根治点。
//
// 接口偏差记录（已裁决采纳，首跳 HTML 导航方案）：计划/brief 假定的
// options.App{URL: serverURL} 字段在 wails v2 全版本线不存在
// （v2.0.0→v2.16.0 的 pkg/options/options.go 均无 URL 字段，生产路径
// app_production.go 亦无任何启动 URL 注入口）。此处采用的等价机制：
//
//	AssetServer{Assets: nil, Handler: HTML 导航页→serverURL}
//
// assetserver 官方语义（pkg/options/assetserver/options.go:15）：
// Assets 为 nil 时所有 GET 转发 Handler → 窗口首跳到真实 server
// （首跳不走 HTTP 302 的原因见 bootRedirectHandler）。此后 WebView
// 全部请求直连 http://127.0.0.1:<port>（Windows 端
// frontend.go processRequest 对非 startURL host 的请求原样放行给
// WebView2；macOS 端 WKWebView 无导航拦截 delegate）——assetserver
// 不接管任何路由，SSE /api/events 仍由真实 HTTP 端口承载，红线守住。
//
// 降级红线：wails.Run 返回 error 原样透传（run 包装为「窗口启动失败:
// %w」→ main 转 slog.Error + 非零退出），绝不静默吞掉后无界面挂着。
// wails.Run 本体不可 headless 测试；可测装配面：buildAppOptions 的
// 窗口参数、首跳导航页组装、OnBeforeClose 接线与弹框参数
// （closeConfirmDialogOptions，window_test.go）；关窗确认三分支判定与
// 默认键平台映射已下沉平台无关纯函数（window_logic.go +
// window_logic_test.go，无 build tag，CI ubuntu 可编译执行）。
//
// Task 7 关窗行为（需求方裁决，双平台一致）：关窗即退出——无活动任务
// 直接放行；有 pending/running 任务弹「您还有任务未完成，确定关闭软件
// 吗？」，取消留在窗口、确认退出进程。不做托盘/最小化驻留。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	stdruntime "runtime"
	"sync/atomic"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// wailsConfig 窗口装配参数（计划冻结值的载体，buildAppOptions 消费）。
type wailsConfig struct {
	title               string
	width, height       int
	minWidth, minHeight int
}

// defaultWailsConfig 计划逐字窗口参数：标题「飞舞」，1280×832，最小 960×600。
func defaultWailsConfig() wailsConfig {
	return wailsConfig{
		title:     "飞舞",
		width:     1280,
		height:    832,
		minWidth:  960,
		minHeight: 600,
	}
}

// confirmClose 关窗确认（Task 7，包级可注入变量——测试替换，defer 恢复；
// 与 launchBrowser 同一注入模式）。语义：返回 true = 放行关闭（用户确认
// 退出进程）；false = 阻止（用户取消，窗口保留）。
//
// 默认实现 runtime.MessageDialog + QuestionDialog：title「飞舞」，文案
// 逐字「您还有任务未完成，确定关闭软件吗？」。跨平台返回语义（wails
// v2.16.0 实际行为）：Windows 端 QuestionDialog 固定 MB_YESNO 两键，
// 返回 "Yes"/"No"（internal/frontend/desktop/windows/dialog.go:196）；
// macOS 端返回被点按钮的 label（desktop/darwin/dialog.go:176）——
// Buttons 提供 macOS 的两个按钮，判定取两平台白名单并集。弹框失败
// 保守策略：返回 false 阻止关闭——宁可让用户再按一次关窗，也绝不因
// 弹框故障误杀下载任务；未知返回值（空串等）同判。
var confirmClose = func(ctx context.Context) bool {
	selected, err := runtime.MessageDialog(ctx, closeConfirmDialogOptions())
	if err != nil {
		slog.Warn("关窗确认弹框失败（保守阻止关闭）", "err", err)
		return false
	}
	return selected == "Yes" || selected == "退出"
}

// closeConfirmDialogOptions 关窗确认弹框参数（纯函数装配面，可测）：
// Type/Title/Message/Buttons/CancelButton 计划冻结值不动；DefaultButton
// 经 defaultCloseConfirmButton（window_logic.go，平台无关映射）按 GOOS
// 分支，守住「回车≠退出」的保守策略：
//
//   - darwin："取消" → 回车=取消=留在窗口；
//   - windows：小写 "no" → 命中 wails MB_DEFBUTTON2，回车=No=不退出
//     （此前固定 "取消" 在 Windows 端静默不生效，属本修复点）。
//
// 全平台映射表断言见 window_logic_test.go；本平台断言见
// window_test.go 的 TestCloseConfirmDialogOptionsDefaultButton。
func closeConfirmDialogOptions() runtime.MessageDialogOptions {
	return runtime.MessageDialogOptions{
		Type:          runtime.QuestionDialog,
		Title:         "飞舞",
		Message:       "您还有任务未完成，确定关闭软件吗？",
		Buttons:       []string{"退出", "取消"},
		DefaultButton: defaultCloseConfirmButton(stdruntime.GOOS),
		CancelButton:  "取消",
	}
}

// buildAppOptions 生成 wails 启动选项（包内可测装配面）：窗口参数取计
// 划冻结值；StartHidden 显式 false——启动即见窗口（本计划目标：直接打
// 开 app 即见界面）；AssetServer 仅提供「HTML 导航页 → serverURL」首跳，
// server URL 原样直传不拼路径，路由全部由真实 HTTP 端口承载（机制见文件头）。
//
// hasActiveTasks 为 webapi.App.HasActiveTasks 的延迟解析闭包（Task 7）：
// OnBeforeClose 无活动任务返回 false 直接放行；有活动任务经 confirmClose
// 确认——确认放行（false）、取消阻止（true）。OnBeforeClose 在 wails
// 事件循环 goroutine 执行，闭包内跨 goroutine 读取的 app 引用由装配方
// （main）以 atomic 保证可见性。
func buildAppOptions(serverURL string, hasActiveTasks func() bool) *options.App {
	cfg := defaultWailsConfig()
	return &options.App{
		Title:       cfg.title,
		Width:       cfg.width,
		Height:      cfg.height,
		MinWidth:    cfg.minWidth,
		MinHeight:   cfg.minHeight,
		StartHidden: false,
		AssetServer: &assetserver.Options{Handler: bootRedirectHandler(serverURL)},
		OnBeforeClose: func(ctx context.Context) bool {
			// 三分支判定已下沉平台无关（window_logic.go 的
			// shouldPreventClose，CI ubuntu 可测）；此处仅把 ctx 包进
			// confirmClose 闭包完成注入。
			return shouldPreventClose(hasActiveTasks, func() bool { return confirmClose(ctx) })
		},
	}
}

// bootRedirectHandler 窗口首跳处理器：输出一页立即导航到 serverURL 的
// HTML（meta refresh + JS location.replace 双保险）。不用 302——darwin
// 资产响应经 WKURLSchemeHandler 投递，不解析 HTTP 重定向语义，真机实证
// 302 body「Found.」被当普通内容渲染成白屏；meta refresh/JS 是标准导航，
// 双平台一致。serverURL 来自 ln.Addr() 构造的 http://127.0.0.1:<port>，
// 不含 HTML 特殊字符，逐字嵌入不转义；原样直传不拼路径，路由全部由真实
// HTTP 端口承载。
func bootRedirectHandler(serverURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8">`+
			`<meta http-equiv="refresh" content="0;url=%s">`+
			`<script>location.replace(%q)</script></head><body></body></html>`, serverURL, serverURL)
	})
}

// newWailsRunner 窗口运行器（main 装配处 withWindowRunner(newWailsRunner(
// signalCtx, hasActiveTasks)) 接线）：阻塞运行 wails 窗口直到关闭，
// wails.Run 的 error 原样透传。非桌面平台（!darwin && !windows）由
// window_other.go 提供对称 nil 空位，run 落入无窗口模式（纯服务端，
// 浏览器仅按 FEIWU_OPEN_BROWSER=1 可选开启）。
//
// ctx 为 main 的 signal.NotifyContext（SIGINT/SIGTERM）：run 阻塞在
// wails.Run 里收不到信号 ctx 取消，故 runner 内起 goroutine 接线——
// <-ctx.Done() → runtime.Quit(wailsCtx) 关窗口，使 wails.Run 返回、
// run 走关窗收尾链。wailsCtx 由 OnStartup 存入（atomic.Value 跨
// goroutine 可见）；信号早于窗口 startup 到达的窗口期由 OnStartup 内
// 的 ctx.Err() 复查补齐。ctx == nil 时（测试/防御）不接线。
//
// serverURL 空串直接返回 error：窗口首跳无目标地址，拒绝开窗。
func newWailsRunner(ctx context.Context, hasActiveTasks func() bool) func(string) error {
	return func(serverURL string) error {
		if serverURL == "" {
			return errors.New("serverURL 为空：窗口首跳地址缺失，拒绝开窗")
		}
		var wailsCtx atomic.Value // context.Context；OnStartup 写，信号 goroutine 读
		opts := buildAppOptions(serverURL, hasActiveTasks)
		opts.OnStartup = func(c context.Context) {
			wailsCtx.Store(c)
			if ctx != nil && ctx.Err() != nil {
				runtime.Quit(c) // 信号早于 startup 到达：立即关窗走收尾
			}
		}
		if ctx != nil {
			go func() {
				<-ctx.Done()
				if c, ok := wailsCtx.Load().(context.Context); ok {
					runtime.Quit(c)
				}
			}()
		}
		return wails.Run(opts)
	}
}
