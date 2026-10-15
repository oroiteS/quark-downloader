//go:build darwin || windows

// Wails 原生窗口运行器（系列 4/5 Task 2）：Wails 仅作窗口壳，窗口直连
// 本地 server 完整 URL——gin + 随机端口架构不动。wails.Run 的 ObjC
// bootstrap 让进程拥有完整 NSApplication 生命周期，是 macOS Dock 弹跳
// 的根治点。
//
// ⚠️ 接口偏差记录（待控制器裁决）：计划/brief 假定的
// options.App{URL: serverURL} 字段在 wails v2 全版本线不存在
// （v2.0.0→v2.16.0 的 pkg/options/options.go 均无 URL 字段，生产路径
// app_production.go 亦无任何启动 URL 注入口）。此处采用的等价机制：
//
//	AssetServer{Assets: nil, Handler: 302→serverURL}
//
// assetserver 官方语义（pkg/options/assetserver/options.go:15）：
// Assets 为 nil 时所有 GET 转发 Handler → 窗口首跳 302 到真实 server。
// 此后 WebView 全部请求直连 http://127.0.0.1:<port>（Windows 端
// frontend.go processRequest 对非 startURL host 的请求原样放行给
// WebView2；macOS 端 WKWebView 无导航拦截 delegate）——assetserver
// 不接管任何路由，SSE /api/events 仍由真实 HTTP 端口承载，红线守住。
//
// 降级红线：wails.Run 返回 error 原样透传（run 包装为「窗口启动失败:
// %w」→ main 转 slog.Error + 非零退出），绝不静默吞掉后无界面挂着。
// wails.Run 本体不可 headless 测试；可测装配面：buildAppOptions 的
// 窗口参数与首跳 302 组装、newWailsRunner 存在性（window_test.go）。
package main

import (
	"net/http"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
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

// buildAppOptions 生成 wails 启动选项（包内可测装配面）：窗口参数取计
// 划冻结值；StartHidden 显式 false——启动即见窗口（本计划目标：直接打
// 开 app 即见界面）；AssetServer 仅提供「302 → serverURL」首跳，server
// URL 原样直传不拼路径，路由全部由真实 HTTP 端口承载（机制见文件头）。
func buildAppOptions(serverURL string) *options.App {
	cfg := defaultWailsConfig()
	return &options.App{
		Title:       cfg.title,
		Width:       cfg.width,
		Height:      cfg.height,
		MinWidth:    cfg.minWidth,
		MinHeight:   cfg.minHeight,
		StartHidden: false,
		AssetServer: &assetserver.Options{Handler: bootRedirectHandler(serverURL)},
	}
}

// bootRedirectHandler 窗口首跳处理器：任意 GET → 302 Location: serverURL。
// serverURL 原样直传（http.Redirect 会做 URL 转义但不改变目标语义），
// WebView 跟随重定向后 origin 即为本地 server。
func bootRedirectHandler(serverURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, serverURL, http.StatusFound)
	})
}

// newWailsRunner 窗口运行器（main 装配处 withWindowRunner(newWailsRunner())
// 接线）：阻塞运行 wails 窗口直到关闭，wails.Run 的 error 原样透传。
// 非桌面平台（!darwin && !windows）由 window_other.go 提供 nil 空位，
// run 落回无窗口模式的 openBrowser 回退。
func newWailsRunner() func(string) error {
	return func(serverURL string) error {
		return wails.Run(buildAppOptions(serverURL))
	}
}
