// Package webapi 飞舞 Web API：gin 骨架（App 生命周期 + 事件总线 + 静态
// 服务）。本文件是应用骨架——后续 8 个任务（settings/session/tasks 等
// API、restore 钩子接线）都在 App 的字段位与 buildRouter 上生长。
package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"quark-downloader/internal/protocol"
	"quark-downloader/internal/store"
)

// Deps App 构造依赖。字段只增不改（后续任务扩展挂载点）。
type Deps struct {
	Paths    store.Paths        // 数据目录定位结果（G7）
	Settings store.Settings     // 启动时加载的设置（Task 13 的 GET/PUT /api/settings 读写它）
	Secrets  *store.SecretStore // Cookie 加密存取（Task 6 会话装配消费）
	Manager  taskManager        // 下载管理器（Task 9 起接线；*engine.Manager 满足，测试注入 fake；不装配则 POST /api/tasks 拒绝）
}

// App 是 feiwu 的应用态容器（裁决 P1）：session/manager/store/settings/
// hub 全部收敛在此，main.go 只做装配与信号接线。后续 Task 7
// （restoreSession）/Task 9（Manager 接线）/Task 11（restoreTasks）/
// Task 12-13（API 端点）都改 App 内部，main.go 不再为此改动。
type App struct {
	paths    store.Paths
	settings store.Settings
	secrets  secretStore // 凭据加密存取（*store.SecretStore 满足；测试注入 fake）
	manager  taskManager // nil = 未装配（POST /api/tasks 拒绝；Close 跳过）

	// 登录流会话组（auth.go 维护）：jar 与 session/auth 同源，
	// resetSession 原子重建三者；newAuthPair 是装配工厂（测试注入点）。
	authMu          sync.Mutex
	jar             *protocol.CookieJar
	session         *protocol.Session // 业务会话（Task 9 直链等）；与 auth 同源
	auth            authAPI           // 登录流操作面（sessionAuth 适配或测试 fake）
	newAuthPair     func(jar *protocol.CookieJar) (*protocol.Session, authAPI)
	qr              qrSession          // 进行中的扫码会话；nil = 无
	keepaliveCancel context.CancelFunc // 保活心跳停止开关；nil = 未启动

	hub    *Hub
	router *gin.Engine
}

// New 装配 App（hub、router 一次建好；Router 返回同一实例）。
// 会话组以生产工厂初始装配（空 jar 未登录态）；Start 时 restoreSession
// 用 SecretStore 里已存的 Cookie 重建登录态并启动保活。
func New(deps Deps) *App {
	a := &App{
		paths:       deps.Paths,
		settings:    deps.Settings,
		secrets:     deps.Secrets,
		manager:     deps.Manager,
		newAuthPair: newAuthPairDefault,
		hub:         NewHub(),
	}
	a.resetSession()
	a.router = a.buildRouter()
	return a
}

// Router 返回应用的 gin 引擎（健康检查、SSE、静态与后续 /api 端点）。
func (a *App) Router() *gin.Engine { return a.router }

// Start 启动钩子：恢复链入口（main 在监听前调用一次）。
//
//	restoreSession（session.go，D9：Cookie 恢复 + keepalive 启动）
//	restoreTasks（Task 11：tasks.json 恢复 + 重新提交，现为空方法位）
func (a *App) Start(ctx context.Context) error {
	if err := a.restoreSession(ctx); err != nil {
		return fmt.Errorf("恢复会话: %w", err)
	}
	if err := a.restoreTasks(ctx); err != nil {
		return fmt.Errorf("恢复任务: %w", err)
	}
	return nil
}

// restoreTasks 任务恢复钩子（Task 11 挂载位）。
func (a *App) restoreTasks(context.Context) error { return nil }

// Close 关闭钩子（main 挂 http.Server.RegisterOnShutdown；Shutdown 开始
// 即触发，让 SSE 长连接先行退出——排空不被 5s 预算拖满后强关）。幂等。
func (a *App) Close() {
	a.authMu.Lock()
	a.stopKeepaliveLocked() // 停保活心跳（§2.6：ctx 取消即停）
	a.authMu.Unlock()
	a.hub.Close()
	if m := a.manager; m != nil {
		// E6 停机保留断点。App.Close 会被 http.Server RegisterOnShutdown
		// 的 go f() 与主关闭链并发调用——不包 once 的依据：engine.
		// Manager.Close 自身幂等且并发安全（closed 检查与设置同处 m.mu
		// 临界区，manager.go:699-710；重复/并发调用直接返回）。
		m.Close()
	}
	// TODO(Task 11): store 落盘（tasks.json 任务上下文快照）。
}

// buildRouter 装配 gin 引擎：
//
//	/api 组（health + events + 后续任务的全部 API）＋错误中间件
//	（统一 {error:{code,message}}；panic → 500 internal，不泄漏堆栈）；
//	静态与 SPA fallback（static.go）；不做 CORS——前端同源 embed。
func (a *App) buildRouter() *gin.Engine {
	r := gin.New()
	r.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeError(c, http.StatusInternalServerError, "internal", "服务器内部错误")
	}))

	api := r.Group("/api")
	api.GET("/health", a.handleHealth)
	api.GET("/events", a.handleEvents)

	// 登录流端点（D8/§6.3，auth.go）。
	api.POST("/login/qr/start", a.handleQRStart)
	api.GET("/login/qr/state", a.handleQRState)
	api.POST("/login/cookie", a.handleCookieImport)
	api.POST("/logout", a.handleLogout)
	api.GET("/account", a.handleAccount)

	// 任务创建（Task 9，tasks.go：目录展开/预算分配/自愈回调）。
	api.POST("/tasks", a.handleCreateTasks)

	// 任务查询与控制（Task 10，tasks.go：列表聚合 + pause/resume/
	// cancel/retry，retry 对被淘汰任务重走创建路径复活）。
	api.GET("/tasks", a.handleListTasks)
	api.POST("/tasks/:id/pause", a.handleTaskPause)
	api.POST("/tasks/:id/resume", a.handleTaskResume)
	api.POST("/tasks/:id/cancel", a.handleTaskCancel)
	api.POST("/tasks/:id/retry", a.handleTaskRetry)

	registerStatic(r)
	r.NoRoute(func(c *gin.Context) {
		// /api（含 /api/ 前缀子路径）的未匹配路径保持 JSON 语义（客户端
		// bug 或版本错配，回落 HTML 只会掩盖）；其余路径走 SPA fallback。
		if p := c.Request.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
			writeError(c, http.StatusNotFound, "not_found", "接口不存在: "+p)
			return
		}
		serveStatic(c)
	})
	return r
}

// handleHealth GET /api/health → 200 {"status":"ok"}（Task 1 验收语义）。
func (a *App) handleHealth(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// sseHeartbeat 心跳间隔（包级变量：测试注入短值验证心跳行；生产 15s）。
var sseHeartbeat = 15 * time.Second

// handleEvents GET /api/events——SSE 事件流：
//
//	Content-Type text/event-stream；事件帧 "data: {json}\n\n"（Event 的
//	JSON 序列化）；心跳为注释行 ": ping\n\n"（15s——保活并穿透中间代理
//	缓冲，注释行前端 EventSource 自动忽略）；客户端断开（request ctx
//	Done）即退订；hub 关闭（服务停机）亦退出。
func (a *App) handleEvents(c *gin.Context) {
	ch, cancel := a.hub.Subscribe()
	defer cancel() // 客户端断开/服务停机统一在此退订（幂等）

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Status(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)
	if flusher == nil { // gin/httptest 的 ResponseWriter 均实现；极端环境降级为一次性输出
		flusher = nopFlusher{}
	}
	flusher.Flush() // 头先行，让客户端立刻拿到 Content-Type

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()
	ctx := c.Request.Context()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return // hub 关闭（停机链）
			}
			b, err := json.Marshal(e)
			if err != nil {
				continue // 序列化失败：丢本帧（不应发生），流不断
			}
			fmt.Fprintf(c.Writer, "data: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(c.Writer, ": ping\n\n")
			flusher.Flush()
		case <-ctx.Done():
			return // 客户端断开
		}
	}
}

// nopFlusher Flusher 空实现（Writer 不可 Flush 时的降级占位）。
type nopFlusher struct{}

func (nopFlusher) Flush() {}

// writeError 统一错误出口：{error:{code,message}}（Task 12+ 全部 /api
// 端点的错误形态；CORS 不做——同源 embed）。
func writeError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{
		"error": gin.H{"code": code, "message": message},
	})
}
