// auth.go 实现登录流端点（设计 D8 + 规格 §6.3）：
// 扫码登录（start/state）、Cookie 文本导入、登出、账号信息。
// App 经 authAPI 小接口消费 protocol 会话——生产由 sessionAuth 适配
// *protocol.Session，测试注入 fake（Task 7/9/11 的会话接线同用此面）。
package webapi

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"net/url"
	"strings"
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

// cookiePersistInterval 是登录态凭据的周期落盘节奏（60s）。包级变量：
// 测试注入短值验证周期写（对齐 sseHeartbeat 的注入先例）。
//
// 背景（控制者实测根因）：夸克 __puus 为轮换型凭据——运行期间协议层
// 随 Set-Cookie 在内存 jar 更新，但 SecretStore 只在登录/导入时写入，
// 重启读回登录瞬间的旧票，严校验接口已不认 → 登录失效。周期落盘把
// 轮换持续同步到持久层（protocol/authcheck.go 的心跳只保鲜内存 jar、
// 不落盘，本层不动 protocol——D9 裁决由 App 层补偿）。
var cookiePersistInterval = 60 * time.Second

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
//
// 凭据周期落盘 goroutine 与心跳同一 ctx 派生：stopKeepaliveLocked/
// resetSession/logout/Close 取消 ctx 即一并停止，无独立生命周期。
func (a *App) startKeepaliveLocked() {
	a.stopKeepaliveLocked()
	ctx, cancel := context.WithCancel(context.Background())
	a.keepaliveCancel = cancel
	auth := a.auth
	auth.StartKeepalive(ctx, func() {
		// expired 帧（Task 12 登录态事件；logged_in=false——保活失败
		// 即会话已失效，§2.6）。
		a.broadcastAuthEvent("expired", false)
	})
	go a.persistCookieLoop(ctx)
}

// persistCookieLoop 周期落盘 goroutine：启动立即落盘一次，之后每
// cookiePersistInterval 落盘一次；ctx 取消即退出。
//
// 立即首轮兼做「启动时立即落盘」：恢复链 restoreSession 成功也经
// startKeepaliveLocked 走到这里——重启后凭据恢复出登录态的同时即把
// 当前 jar 重新写回持久层，消除「登录后没重启过但轮换已发生」窗口
// （上一次运行周期写下的票可能在停机前最后 60s 内又轮换过）。登录
// 链（扫码/导入）刚同步写过同内容，这一轮是幂等冗余写，成本一次
// keyring/文件写，可接受。
func (a *App) persistCookieLoop(ctx context.Context) {
	ticker := time.NewTicker(cookiePersistInterval)
	defer ticker.Stop()
	for {
		a.persistCookieOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// persistCookieOnce 落盘一次当前凭据。全程持 authMu：「本代存活检查 +
// jar 快照 + 登录态判定 + SaveCookie」在同一临界区内一气呵成——快照与
// 写入之间没有 authMu 空窗，logout 清场（ctx 取消与 SaveCookie("") 同在
// logout 的 authMu 临界区内）与本次落盘严格串行，旧票不可能在清场后
// 复活已删凭据。与既有登录链 handleQRState/handleCookieImport 持 authMu
// 调 SaveCookie 同模式（fix round 1 控制者裁决）：锁序 authMu→store.mu
// 单向无环，SecretStore 方法封闭、从不反向等 authMu，无死锁；代价是
// handler 临界区潜在多等一次 ms 级 keyring/文件 IO，可接受。
//
//   - ctx 已取消（本代已被 stop/resetSession/logout 停掉）→ 不写：
//     与 logout 临界区互斥，本代死后任何轮次都不再触碰 Store；
//   - jar 无 __puus（LoginState false）→ 跳过：防把好凭据覆盖为空；
//     空串只由 logout 显式写入（原路径），本 goroutine 永不写空。
func (a *App) persistCookieOnce(ctx context.Context) {
	a.authMu.Lock()
	defer a.authMu.Unlock()

	if ctx.Err() != nil {
		return
	}
	if !a.auth.LoginState() {
		return
	}
	if err := a.secrets.SaveCookie(a.jar.String()); err != nil {
		// 周期写失败不致命（凭据仍是上一轮的票）：记一行，下轮重试。
		log.Printf("webapi: 凭据周期落盘失败（保留上一轮凭据，下轮重试）: %v", err)
	}
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
		writeError(c, http.StatusBadGateway, codeQRStartFailed,
			"获取扫码 token 失败: "+err.Error())
		return
	}
	scanURL := qr.ScanURL()
	u, perr := url.Parse(scanURL)
	if perr != nil || u.Query().Get("token") == "" {
		writeError(c, http.StatusInternalServerError, codeInternal,
			"扫码页 URL 缺少 token: "+scanURL)
		return
	}
	png, err := protocol.QRPNG(scanURL, qrPNGSize)
	if err != nil {
		writeError(c, http.StatusInternalServerError, codeInternal,
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
		writeError(c, http.StatusBadRequest, codeNoQRSession,
			"没有进行中的扫码登录会话，请先 POST /api/login/qr/start")
		return
	}
	state, ticket, err := a.qr.Poll(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusBadRequest, codeQRPollFailed,
			"扫码轮询失败: "+err.Error())
		return
	}

	switch state {
	case protocol.QRGotTicket:
		a.qr = nil // ticket 已消费，会话终结
		info, err := a.auth.ExchangeCookie(c.Request.Context(), ticket)
		if err != nil {
			writeError(c, http.StatusInternalServerError, codeExchangeFailed,
				"换取登录 Cookie 失败: "+err.Error())
			return
		}
		if !a.auth.LoginState() {
			// 跨链判据统一（§2.6 __puus 唯一判据）：换 Cookie 协议成功
			// 返回但 jar 缺 __puus（上游未下发登录态）时不给 logged_in——
			// 与导入链/恢复链同一判据，杜绝"协议成功=已登录"的误判。
			writeError(c, http.StatusInternalServerError, codeExchangeFailed,
				"换取的 Cookie 缺少 __puus（登录态 Cookie），请重新扫码登录")
			return
		}
		if err := a.secrets.SaveCookie(a.jar.String()); err != nil {
			writeError(c, http.StatusInternalServerError, codeSaveCookieFailed,
				"保存登录凭据失败: "+err.Error())
			return
		}
		a.refreshAccountCacheLocked(info) // 昵称/头像双层缓存（进程内 + 盘）：account 的 QuickCheck 走兜底拿不到 info 时回退（T14 真机发现 B；T15 升级为跨重启持久；持 authMu 临界区内调 Locked 变体）
		a.startKeepaliveLocked()
		a.broadcastAuthEvent("login", true) // Task 12：登录成功广播 auth 帧
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
		writeError(c, http.StatusBadRequest, codeInvalidCookie,
			`请求体须为 {"text":"<Cookie 文本>"} 的 JSON 对象`)
		return
	}
	cookieStr, err := parseCookieText(req.Text)
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidCookie, err.Error())
		return
	}

	a.authMu.Lock()
	defer a.authMu.Unlock()

	a.jar.LoadFromString(cookieStr)
	if !a.auth.LoginState() {
		a.resetSession() // 回滚脏 jar：导入失败不留半登录态
		writeError(c, http.StatusBadRequest, codeInvalidCookie,
			"导入的 Cookie 缺少 __puus（登录态 Cookie），无法登录")
		return
	}
	if err := a.secrets.SaveCookie(a.jar.String()); err != nil {
		writeError(c, http.StatusInternalServerError, codeSaveCookieFailed,
			"保存登录凭据失败: "+err.Error())
		return
	}
	// 昵称/头像缓存与扫码链同权（T15）：导入链没有 ExchangeCookie 的顺带
	// info，快速校验一次补取（§2.6 快端点）；失败不阻断导入——昵称只是
	// 显示件，重启后 handleAccount 的 QuickCheck 成功路径仍会刷新。
	if _, info, err := a.auth.QuickCheck(c.Request.Context()); err == nil &&
		(info.Nickname != "" || info.AvatarURI != "") {
		a.refreshAccountCacheLocked(info) // 持 authMu 临界区内调 Locked 变体（T15）
	}
	a.startKeepaliveLocked()
	a.broadcastAuthEvent("login", true) // Task 12：登录成功广播 auth 帧
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
		writeError(c, http.StatusInternalServerError, codeLogoutFailed,
			"清除登录凭据失败: "+err.Error())
		return
	}
	a.qr = nil
	a.resetSession()
	a.broadcastAuthEvent("logout", false) // Task 12：登录态事件
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
		writeError(c, http.StatusUnauthorized, codeNotLoggedIn, "未登录")
		return
	}
	ok, info, _ := auth.QuickCheck(c.Request.Context())
	if !ok {
		writeError(c, http.StatusUnauthorized, codeNotLoggedIn,
			"登录已失效，请重新扫码登录")
		return
	}
	// QuickCheck 拿到非空 info：刷新双层缓存（进程内 + 盘，T15）——把
	// 协议层现值同步到持久层，供下次重启后的回退链消费。
	if info.Nickname != "" || info.AvatarURI != "" {
		a.refreshAccountCache(info)
	}
	// QuickCheck wall 域失败走 probeFileSort 兜底时 info 为零值（协议层
	// 规格：兜底拿不到账号信息）——昵称/头像回退链（T14 发现 B；T15 扩
	// 展为跨重启）：进程内 cachedAccount（登录链缓存）→ account.json（盘，
	// 重启幸存）；读到非空昵称即用，两者皆空则保持空（前端占位兜底）。
	if info.Nickname == "" && info.AvatarURI == "" {
		a.authMu.Lock()
		cached := a.cachedAccount
		a.authMu.Unlock()
		if cached.Nickname == "" {
			if disk := loadAccountProfile(a.paths.DataDir); disk.Nickname != "" {
				cached = protocol.AccountInfo{
					Nickname:  disk.Nickname,
					AvatarURI: disk.Avatar,
				}
			}
		}
		if cached.Nickname != "" {
			info = cached
		}
	}
	total, used, err := auth.Capacity(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusBadGateway, codeCapacityFailed,
			"查询网盘容量失败: "+err.Error())
		return
	}
	// 头像输出侧升级 https（真机 ATS 修复）：夸克账号头像 URL 为明文
	// http://image.quark.cn/...，macOS WKWebView 的 ATS 默认拦截明文 HTTP
	// 远程资源（loopback 127.0.0.1 有豁免故页面正常，image.quark.cn 被
	// 拦）→ img onerror → 前端回退首字占位。升级只发生在输出侧，缓存
	// （进程内 + account.json）仍存上游原值。
	c.JSON(http.StatusOK, gin.H{
		"logged_in": true,
		"nickname":  info.Nickname,
		"avatar":    secureAvatarURL(info.AvatarURI),
		"capacity":  gin.H{"total": total, "used": used},
	})
}

// secureAvatarURL 把账号头像 URL 输出侧升级为 https：头像域固定为明文
// http://image.quark.cn/...，而 macOS WKWebView 的 ATS（App Transport
// Security）默认拦截明文 HTTP 远程资源（loopback 127.0.0.1 有豁免故页
// 面本身正常，image.quark.cn 被拦）→ img onerror → 前端回退首字占位。
// https://image.quark.cn 已实证可用（200），故仅对精确域前缀做替换；
// 前缀不匹配（非该域/已是 https/相对路径等）原样返回，不做通用改写。
func secureAvatarURL(u string) string {
	const plain, tls = "http://image.quark.cn/", "https://image.quark.cn/"
	if strings.HasPrefix(u, plain) {
		return tls + u[len(plain):]
	}
	return u
}
