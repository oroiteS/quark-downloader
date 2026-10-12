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

// SetManager 启动期一次性注入任务管理器（Task 12 装配形态：main 先建
// App（hub 就绪）→ NewManager(cfg, app.EngineEvent) → SetManager →
// Start——engine 事件由此直连 hub 广播，P4 Ruling 落地）。仅限启动前
// 调用：运行中替换无并发保护（生产装配与测试注入都不需要）。
func (a *App) SetManager(m taskManager) { a.manager = m }

// Start 启动钩子：恢复链入口（main 在监听前调用一次）。
//
//	restoreSession（session.go，D9：Cookie 恢复 + keepalive 启动）
//	restoreTasks（resume.go，Task 11：tasks.json 恢复 + 重新提交）
func (a *App) Start(ctx context.Context) error {
	if err := a.restoreSession(ctx); err != nil {
		return fmt.Errorf("恢复会话: %w", err)
	}
	if err := a.restoreTasks(ctx); err != nil {
		return fmt.Errorf("恢复任务: %w", err)
	}
	return nil
}

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
		writeError(c, http.StatusInternalServerError, codeInternal, "服务器内部错误")
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

	// 分享解析与网盘浏览（Task 13，share.go：parse 建内存会话 sid 引用，
	// files 经会话导航；drive 直透）。
	api.POST("/share/parse", a.handleShareParse)
	api.GET("/share/files", a.handleShareFiles)
	api.GET("/drive/files", a.handleDriveFiles)

	// 直链获取与导出（Task 13，links.go：打分透出 + expires_hint；
	// export 五格式文本）。
	api.POST("/links", a.handleLinks)
	api.POST("/links/export", a.handleLinksExport)

	// 设置读写（Task 13，settings.go：PUT 热应用——destDir 同步
	// manager（T13-R1）、threads/max 即时生效于后续预算（T13-R2））。
	api.GET("/settings", a.handleSettingsGet)
	api.PUT("/settings", a.handleSettingsPut)

	registerStatic(r)
	r.NoRoute(func(c *gin.Context) {
		// /api（含 /api/ 前缀子路径）的未匹配路径保持 JSON 语义（客户端
		// bug 或版本错配，回落 HTML 只会掩盖）；其余路径走 SPA fallback。
		if p := c.Request.URL.Path; p == "/api" || strings.HasPrefix(p, "/api/") {
			writeError(c, http.StatusNotFound, codeNotFound, "接口不存在: "+p)
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

	// 断线对账（G3，Task 12）：订阅即发 hello 帧带全量任务快照（与
	// GET /api/tasks 同源组装）——重连客户端首帧即恢复视图，不必先拉
	// REST 对账（Focus #4；REST 拉取仍是等价兜底）。
	if b, err := json.Marshal(Event{Type: "hello", Payload: a.tasksSnapshotJSON()}); err == nil {
		fmt.Fprintf(c.Writer, "data: %s\n\n", b)
		flusher.Flush()
	}

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

// ---- 冻结的错误码契约（Task 12，系列 4 前端依赖）----
//
// 常量值即 wire 契约：重命名/增删须同步前端（TestErrorCodeConstantsFrozen
// 是唯一权威清单的护栏）。engine sentinel 映射：ErrInvalidThreads→
// invalid_threads（400 参数错误）、ErrDestBusy→dest_busy（创建路径进
// skipped 不升 HTTP 错误）、ErrClosed→manager_closed（503）。

const (
	codeNotLoggedIn         = "not_logged_in"         // 401 未登录（§2.6 __puus 判据）
	codeInternal            = "internal"              // 500 内部错误（panic 兜底同码）
	codeNotFound            = "not_found"             // 404 路由不存在
	codeTaskNotFound        = "task_not_found"        // 404 任务双 miss（引擎+TaskCtx）
	codeNeedsReparse        = "needs_reparse"         // 409 需重新解析（D9）
	codeDestBusy            = "dest_busy"             // 409 同路径互斥（§3.13；创建链进 skipped）
	codeSubmitFailed        = "submit_failed"         // 502 Submit 失败（非参数/互斥/关闭类）
	codeInvalidThreads      = "invalid_threads"       // 400 并发档位非法（ErrInvalidThreads）
	codeManagerClosed       = "manager_closed"        // 503 管理器关闭（ErrClosed）
	codeInvalidTasksRequest = "invalid_tasks_request" // 400 创建请求参数错误
	codeSaveTasksFailed     = "save_tasks_failed"     // 500 tasks.json 落盘失败
	codeSaveCookieFailed    = "save_cookie_failed"    // 500 凭据保存失败
	codeLogoutFailed        = "logout_failed"         // 500 登出清理失败
	codeExchangeFailed      = "exchange_failed"       // 500 ticket 换 Cookie 失败
	codeQRStartFailed       = "qr_start_failed"       // 502 发起扫码失败
	codeQRPollFailed        = "qr_poll_failed"        // 500 轮询扫码状态失败
	codeNoQRSession         = "no_qr_session"         // 400 无进行中的扫码会话
	codeInvalidCookie       = "invalid_cookie"        // 400 Cookie 导入格式/内容非法
	codeShareTokenFailed    = "share_token_failed"    // 502 换取分享凭证失败
	codeLinksFailed         = "links_failed"          // 502 批量取直链失败
	codeExpandFailed        = "expand_failed"         // 502 目录展开失败
	codeCapacityFailed      = "capacity_failed"       // 502 网盘容量查询失败
	// Task 13 增码（29 项冻结）：解析/网盘/直链/设置端点的错误面。
	codeInvalidShareText   = "invalid_share_text"    // 400 分享文案无链接/口令不可解析
	codeShareListFailed    = "share_list_failed"     // 502 分享目录列表失败
	codeDriveListFailed    = "drive_list_failed"     // 502 网盘目录列表失败
	codeNoShareSession     = "no_share_session"      // 404 分享会话 sid 无效/过期
	codeInvalidLinksReq    = "invalid_links_request" // 400 直链/导出请求参数错误
	codeInvalidSettings    = "invalid_settings"      // 400 设置值非法（theme/档位/目录）
	codeSaveSettingsFailed = "save_settings_failed"  // 500 settings.json 落盘失败
)
