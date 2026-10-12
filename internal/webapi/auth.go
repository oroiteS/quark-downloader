// auth.go 实现登录流端点（设计 D8 + 规格 §6.3）：
// 扫码登录（start/state）、Cookie 文本导入、登出、账号信息。
// App 经 authAPI 小接口消费 protocol 会话——生产由 sessionAuth 适配
// *protocol.Session，测试注入 fake（Task 7/9/11 的会话接线同用此面）。
package webapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"

	"quark-downloader/internal/protocol"
)

// qrPollInterval 是前端扫码轮询节奏（§2.4 实测常数表：1.2s/次）。
//
// ⚠️ 节奏契约钉死：/api/login/qr/state 是单发 Poll 语义——每次 HTTP 调用
// 恰好驱动一轮协议轮询，服务端不自旋、不缓存、不推送中间态；前端以
// setInterval(1200ms) 轮询本端点实现规格节奏，两侧都不改。
// （文档性常量：服务端不据此定时，作为前后端与规格三方对齐的锚点。）
const qrPollInterval = 1200 * time.Millisecond

// qrPNGSize 登录二维码 PNG 边长（像素，简报钉死 512）。
const qrPNGSize = 512

// qrSession 是一次扫码登录会话的最小操作面（protocol.QRLogin 满足）。
// 接口化仅为测试可注入编排式 fake Poll 序列。
type qrSession interface {
	// Poll 推进一轮状态机（单发语义，轮询节奏 1.2s 由前端驱动）。
	Poll(ctx context.Context) (protocol.QRState, string, error)
	// ScanURL 返回扫码页 URL（§2.2 逐字模板）。
	ScanURL() string
}

// authAPI 是 webapi 消费 protocol 会话的登录流操作面。包内小接口：
// *protocol.Session 经 sessionAuth 适配满足；测试注入 fake。
// Task 7 的 restoreSession/keepalive 注入化、Task 9 的业务接线都在此面生长。
type authAPI interface {
	// StartQRLogin 取二维码 token，开启一次扫码会话。
	StartQRLogin(ctx context.Context) (qrSession, error)
	// ExchangeCookie 用 service_ticket 换登录 Cookie（§2.5）。
	ExchangeCookie(ctx context.Context, ticket string) (protocol.AccountInfo, error)
	// QuickCheck 快速校验登录态（§2.6），顺带取昵称/头像。
	QuickCheck(ctx context.Context) (bool, protocol.AccountInfo, error)
	// Capacity 查询网盘容量（§2.8 ⑨）。
	Capacity(ctx context.Context) (total, used int64, err error)
	// LoginState 报告是否登录态（jar 含 __puus，§2.6）。
	LoginState() bool
	// StartKeepalive 启动保活心跳（§2.6），ctx 取消即停。
	StartKeepalive(ctx context.Context, onExpire func())
}

// secretStore 是凭据存取的最小面（*store.SecretStore 满足；测试注入 fake，
// 避免测试环境触碰真实 keyring/磁盘）。
type secretStore interface {
	SaveCookie(cookieStr string) error
	LoadCookie() string
}

// sessionAuth 把 *protocol.Session 适配成 authAPI。独立 adapter 的唯一
// 原因：StartQRLogin 的具体返回类型 *protocol.QRLogin 与 qrSession 接口
// 签名不同，须装箱一次。
type sessionAuth struct{ s *protocol.Session }

func (a sessionAuth) StartQRLogin(ctx context.Context) (qrSession, error) {
	l, err := a.s.StartQRLogin(ctx)
	if err != nil {
		return nil, err
	}
	return l, nil
}

func (a sessionAuth) ExchangeCookie(ctx context.Context, ticket string) (protocol.AccountInfo, error) {
	return a.s.ExchangeCookie(ctx, ticket)
}

func (a sessionAuth) QuickCheck(ctx context.Context) (bool, protocol.AccountInfo, error) {
	return a.s.QuickCheck(ctx)
}

func (a sessionAuth) Capacity(ctx context.Context) (int64, int64, error) {
	return a.s.Capacity(ctx)
}

func (a sessionAuth) LoginState() bool { return a.s.LoginState() }

func (a sessionAuth) StartKeepalive(ctx context.Context, onExpire func()) {
	a.s.StartKeepalive(ctx, onExpire)
}

// newAuthPairDefault 生产会话工厂：新 jar 上建真 Session 并适配 authAPI。
// App.resetSession 消费；测试经 App.newAuthPair 整体替换注入 fake。
func newAuthPairDefault(jar *protocol.CookieJar) (*protocol.Session, authAPI) {
	s := protocol.NewSession(nil, jar)
	return s, sessionAuth{s}
}

// resetSession 重建内存会话：新 CookieJar + 新 Session + 重适配 auth。
// New 的初始装配、logout 清场与导入/恢复失败回滚共用——CookieJar 没有
// 清空方法，重实例化即"清空"（旧实例随旧 session 一并弃用）。
//
// 调用方须持 authMu（New 首次调用时尚无并发，裸调安全）。重建时：
//  1. 先停旧 keepalive——弃用 session 上的心跳不再空跑（§2.6 取消即停）；
//  2. 挂鉴权自愈（§2.3/D9）：DoAPI 遇 401/403/412 时经本代 authAPI
//     快速校验登录态，Cookie 仍有效则重试原请求一次。闭包捕获本代
//     auth（与 session 同代），logout 重建后旧代 session 的自愈不会
//     误读新代状态。测试工厂只给 fake authAPI（session 为 nil）时跳过
//     ——heal 挂在 *protocol.Session 上，无 session 无从挂起。
func (a *App) resetSession() {
	a.stopKeepaliveLocked()
	a.jar = protocol.NewCookieJar()
	a.session, a.auth = a.newAuthPair(a.jar)
	if s := a.session; s != nil {
		auth := a.auth
		s.SetAuthHeal(func(ctx context.Context) bool {
			ok, _, _ := auth.QuickCheck(ctx)
			return ok
		})
	}
}

// startKeepaliveLocked 启动保活心跳（先停旧——登录成功重复触发时
// 保证全进程至多一个心跳，对齐 protocol"调用方负责不重复启动"的约定）。
// onExpire 只广播 auth 事件通知前端重新登录，会话清理留给用户操作
// （重新登录覆盖或 logout）。
func (a *App) startKeepaliveLocked() {
	a.stopKeepaliveLocked()
	ctx, cancel := context.WithCancel(context.Background())
	a.keepaliveCancel = cancel
	payload, _ := json.Marshal(map[string]bool{"logged_in": false})
	auth := a.auth
	auth.StartKeepalive(ctx, func() {
		a.hub.Broadcast(Event{Type: "auth", Payload: payload})
	})
}

// stopKeepaliveLocked 停保活心跳（§2.6：ctx 取消即停）。幂等。
func (a *App) stopKeepaliveLocked() {
	if a.keepaliveCancel != nil {
		a.keepaliveCancel()
		a.keepaliveCancel = nil
	}
}

// ---- POST /api/login/qr/start ----

// handleQRStart 发起扫码登录：取 token → QRPNG 512 → {token, qrcode_png,
// scan_url}。重复调用 = 刷新二维码（旧会话作废）。
func (a *App) handleQRStart(c *gin.Context) {
	a.authMu.Lock()
	defer a.authMu.Unlock()

	qr, err := a.auth.StartQRLogin(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusBadGateway, "qr_start_failed",
			"获取扫码 token 失败: "+err.Error())
		return
	}
	scanURL := qr.ScanURL()
	u, perr := url.Parse(scanURL)
	if perr != nil || u.Query().Get("token") == "" {
		writeError(c, http.StatusInternalServerError, "internal",
			"扫码页 URL 缺少 token: "+scanURL)
		return
	}
	png, err := protocol.QRPNG(scanURL, qrPNGSize)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "internal",
			"生成登录二维码失败: "+err.Error())
		return
	}
	a.qr = qr
	c.JSON(http.StatusOK, gin.H{
		"token":      u.Query().Get("token"),
		"qrcode_png": base64.StdEncoding.EncodeToString(png),
		"scan_url":   scanURL,
	})
}

// ---- GET /api/login/qr/state ----

// handleQRState 扫码轮询端点：每次调用驱动一次 Poll（单发语义）。
// 六态映射（§2.4）→ {state: waiting|scanned|got_ticket|invalid|expired|
// timeout, logged_in, nickname?}。
//
//	GotTicket → 服务端自动链（简报逐字）：ExchangeCookie → SaveCookie
//	  （SecretStore）→ StartKeepalive → 返回 {state:"got_ticket",
//	  logged_in:true, nickname}；ticket 一次性，会话终结。
//	Invalid/Expired/Timeout 终态 → 会话清除（再轮询 400，须重新 start）。
//	Waiting/Scanned → 会话保留，等下一轮。
func (a *App) handleQRState(c *gin.Context) {
	a.authMu.Lock()
	defer a.authMu.Unlock()

	if a.qr == nil {
		writeError(c, http.StatusBadRequest, "no_qr_session",
			"没有进行中的扫码登录会话，请先 POST /api/login/qr/start")
		return
	}
	state, ticket, err := a.qr.Poll(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusBadRequest, "qr_poll_failed",
			"扫码轮询失败: "+err.Error())
		return
	}

	switch state {
	case protocol.QRGotTicket:
		a.qr = nil // ticket 已消费，会话终结
		info, err := a.auth.ExchangeCookie(c.Request.Context(), ticket)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "exchange_failed",
				"换取登录 Cookie 失败: "+err.Error())
			return
		}
		if !a.auth.LoginState() {
			// 跨链判据统一（§2.6 __puus 唯一判据）：换 Cookie 协议成功
			// 返回但 jar 缺 __puus（上游未下发登录态）时不给 logged_in——
			// 与导入链/恢复链同一判据，杜绝"协议成功=已登录"的误判。
			writeError(c, http.StatusInternalServerError, "exchange_failed",
				"换取的 Cookie 缺少 __puus（登录态 Cookie），请重新扫码登录")
			return
		}
		if err := a.secrets.SaveCookie(a.jar.String()); err != nil {
			writeError(c, http.StatusInternalServerError, "save_cookie_failed",
				"保存登录凭据失败: "+err.Error())
			return
		}
		a.startKeepaliveLocked()
		c.JSON(http.StatusOK, gin.H{
			"state":     "got_ticket",
			"logged_in": true,
			"nickname":  info.Nickname,
		})
	case protocol.QRWaiting, protocol.QRScanned:
		c.JSON(http.StatusOK, gin.H{
			"state":     qrStateName(state),
			"logged_in": a.auth.LoginState(),
		})
	default: // Invalid/Expired/Timeout 终态：会话清除，前端引导重新 start
		a.qr = nil
		c.JSON(http.StatusOK, gin.H{
			"state":     qrStateName(state),
			"logged_in": a.auth.LoginState(),
		})
	}
}

// qrStateName 把 protocol 六态枚举映射为端点契约的状态名（简报逐字）。
func qrStateName(s protocol.QRState) string {
	switch s {
	case protocol.QRWaiting:
		return "waiting"
	case protocol.QRScanned:
		return "scanned"
	case protocol.QRGotTicket:
		return "got_ticket"
	case protocol.QRInvalid:
		return "invalid"
	case protocol.QRExpired:
		return "expired"
	default: // QRTimeout；枚举封闭，其余值实际不可达
		return "timeout"
	}
}

// ---- POST /api/login/cookie ----

// handleCookieImport Cookie 文本导入（§6.3）：四格式解析（①按 domain 过滤
// 只留含 quark 条目）→ jar.LoadFromString → LoginState 校验（__puus）→
// 通过则 SaveCookie + StartKeepalive，否则 400 带原因（且回滚 jar，
// 不留半登录态）。
func (a *App) handleCookieImport(c *gin.Context) {
	var req struct {
		Text string `json:"text"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid_cookie",
			`请求体须为 {"text":"<Cookie 文本>"} 的 JSON 对象`)
		return
	}
	cookieStr, err := parseCookieText(req.Text)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_cookie", err.Error())
		return
	}

	a.authMu.Lock()
	defer a.authMu.Unlock()

	a.jar.LoadFromString(cookieStr)
	if !a.auth.LoginState() {
		a.resetSession() // 回滚脏 jar：导入失败不留半登录态
		writeError(c, http.StatusBadRequest, "invalid_cookie",
			"导入的 Cookie 缺少 __puus（登录态 Cookie），无法登录")
		return
	}
	if err := a.secrets.SaveCookie(a.jar.String()); err != nil {
		writeError(c, http.StatusInternalServerError, "save_cookie_failed",
			"保存登录凭据失败: "+err.Error())
		return
	}
	a.startKeepaliveLocked()
	c.JSON(http.StatusOK, gin.H{"logged_in": true})
}

// ---- POST /api/logout ----

// handleLogout 登出三清（简报逐字）：清 jar、删 SecretStore 凭据、
// 停 keepalive（§2.6：ctx 取消即停）；进行中的扫码会话一并作废。
// 凭据删除以 SaveCookie("") 覆盖实现——SecretStore 无 Delete 入口，
// 空串读取即按未登录处理（§6.1 语义闭环）。
func (a *App) handleLogout(c *gin.Context) {
	a.authMu.Lock()
	defer a.authMu.Unlock()

	a.stopKeepaliveLocked()
	if err := a.secrets.SaveCookie(""); err != nil {
		writeError(c, http.StatusInternalServerError, "logout_failed",
			"清除登录凭据失败: "+err.Error())
		return
	}
	a.qr = nil
	a.resetSession()
	c.JSON(http.StatusOK, gin.H{"logged_in": false})
}

// ---- GET /api/account ----

// handleAccount 账号信息：未登录（无 __puus）或 QuickCheck 判失效 →
// 401 code not_logged_in；已登录返回 QuickCheck（昵称/头像）+ Capacity。
func (a *App) handleAccount(c *gin.Context) {
	a.authMu.Lock()
	auth := a.auth
	a.authMu.Unlock() // 网络调用不持锁；logout 并发重建时用本帧快照

	if !auth.LoginState() {
		writeError(c, http.StatusUnauthorized, "not_logged_in", "未登录")
		return
	}
	ok, info, _ := auth.QuickCheck(c.Request.Context())
	if !ok {
		writeError(c, http.StatusUnauthorized, "not_logged_in",
			"登录已失效，请重新扫码登录")
		return
	}
	total, used, err := auth.Capacity(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusBadGateway, "capacity_failed",
			"查询网盘容量失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"logged_in": true,
		"nickname":  info.Nickname,
		"avatar":    info.AvatarURI,
		"capacity":  gin.H{"total": total, "used": used},
	})
}
