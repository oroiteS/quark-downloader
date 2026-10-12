// share.go 分享解析与网盘浏览端点（Task 13，G 系列浏览闭环）：
//
//	POST /api/share/parse —— 粘贴文案 → 口令如实提示 / 无链接 400 /
//	  正常链 ShareToken 现换 + 根目录列表 → 内存会话 sid 引用返回
//	  （stoken 不透出明文，§6.2 安全取舍：会话留在服务端内存，前端持
//	  sid 续用——泄面最小，代价是重启后会话失效须重新 parse）
//	GET /api/share/files?sid=&pdir_fid= —— 经会话导航子目录（stoken
//	  由会话持有；条目的 share_fid_token 并入会话缓存，供 POST /api/links
//	  免转存三参补齐）
//	GET /api/drive/files?pdir_fid= —— 我的网盘 DriveList 直透
//
// 会话缓存取舍：map 无淘汰（前端会话短生命周期、单用户单实例应用；
// 重启清空），超界防御不做——YAGNI，注释说明。
package webapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"quark-downloader/internal/protocol"
)

// shareSession 一次 parse 建立的服务端会话。
type shareSession struct {
	pwdID      string
	passcode   string
	stoken     string
	tokenCache map[string]string             // fid → share_fid_token（parse/files 列表逐次并入）
	entryCache map[string]protocol.FileEntry // fid → 文件条目（name/size/token；POST sid 引用时文件 fid 直建，未命中才走目录展开）
}

// shareSessions 包级会话表（单实例应用前提，与 tasksFileMu 同型先例；
// 不挂 App 字段——服务端内存即生命周期，重启即失效）。
var (
	shareSessionsMu sync.Mutex
	shareSessions   = map[string]*shareSession{}
)

// newShareSID 随机 16 hex 会话 id（不可猜）。
func newShareSID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sessionSnapshot 会话快照（拷贝 stoken/pwdID，避免持锁打网络）。
func (s *shareSession) snapshot() (pwdID, passcode, stoken string) {
	return s.pwdID, s.passcode, s.stoken
}

// cacheTokens 把列表条目的 share_fid_token 并入缓存（锁内）。
func (s *shareSession) cacheTokens(entries []protocol.FileEntry) {
	for _, e := range entries {
		if e.ShareFIDToken != "" {
			s.tokenCache[e.FID] = e.ShareFIDToken
		}
		if !e.Dir && e.FID != "" {
			s.entryCache[e.FID] = e
		}
	}
}

// shareParseRequest POST /api/share/parse 请求体。
type shareParseRequest struct {
	Text string `json:"text"`
}

// requireSession 网络会话墙（与 POST /api/tasks 同快照模式）：锁内取
// session/auth/jar 三元组，网络调用不持锁。未登录时已写 401 并返回
// ok=false。
func (a *App) requireSession(c *gin.Context) (*protocol.Session, string, bool) {
	a.authMu.Lock()
	sess, auth := a.session, a.auth
	cookieStr := a.jar.String()
	a.authMu.Unlock()
	if sess == nil || !auth.LoginState() {
		writeError(c, http.StatusUnauthorized, codeNotLoggedIn, "未登录")
		return nil, "", false
	}
	return sess, cookieStr, true
}

// handleShareParse POST /api/share/parse——见文件头注释。
func (a *App) handleShareParse(c *gin.Context) {
	var req shareParseRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.Text == "" {
		writeError(c, http.StatusBadRequest, codeInvalidShareText, "请求须带非空 text 字段")
		return
	}
	ref, err := protocol.ParseShareText(req.Text)
	if err != nil {
		// 唯一错误形态：既无分享链接也无口令（brief：无链接→400）。
		writeError(c, http.StatusBadRequest, codeInvalidShareText, "未识别到分享链接")
		return
	}
	if ref.PwdID == "" {
		// 仅口令（无链接可解析）：口令无公开解析接口，如实引导
		// （brief 逐字提示文案）。PwdID 优先：混合文本（口令标记+
		// 有效链接）走下方正常解析链——有链接就不该被口令提示拒绝。
		if !ref.IsKouling {
			writeError(c, http.StatusBadRequest, codeInvalidShareText, "未识别到分享链接")
			return
		}
		c.JSON(http.StatusOK, gin.H{"kouling": true, "hint": "口令请改用分享链接"})
		return
	}

	sess, _, ok := a.requireSession(c)
	if !ok {
		return
	}

	ctx := c.Request.Context()
	stoken, err := sess.ShareToken(ctx, ref.PwdID, ref.Passcode)
	if err != nil {
		writeError(c, http.StatusBadGateway, codeShareTokenFailed, "换取分享凭证失败: "+err.Error())
		return
	}
	files, err := sess.ShareList(ctx, ref.PwdID, stoken, ref.DirFID)
	if err != nil {
		writeError(c, http.StatusBadGateway, codeShareListFailed, "拉取分享根目录失败: "+err.Error())
		return
	}

	// 建会话（根列表 token 并入缓存——links 免转存三参的 token 来源）。
	s := &shareSession{
		pwdID:      ref.PwdID,
		passcode:   ref.Passcode,
		stoken:     stoken,
		tokenCache: map[string]string{},
		entryCache: map[string]protocol.FileEntry{},
	}
	s.cacheTokens(files)
	sid := newShareSID()
	shareSessionsMu.Lock()
	shareSessions[sid] = s
	shareSessionsMu.Unlock()

	c.JSON(http.StatusOK, gin.H{
		"sid":      sid,
		"pwd_id":   ref.PwdID,
		"passcode": ref.Passcode, // 前端续用建任务（POST /api/tasks 直传契约）
		"files":    files,
		// stoken 绝不透出（契约：sid 引用替代）。
	})
}

// handleShareFiles GET /api/share/files?sid=&pdir_fid=——经会话导航。
func (a *App) handleShareFiles(c *gin.Context) {
	sid := c.Query("sid")
	if sid == "" {
		writeError(c, http.StatusBadRequest, codeNoShareSession, "缺少 sid 参数")
		return
	}
	shareSessionsMu.Lock()
	sessn := shareSessions[sid]
	shareSessionsMu.Unlock()
	if sessn == nil {
		writeError(c, http.StatusNotFound, codeNoShareSession, "分享会话不存在或已失效, 请重新解析")
		return
	}

	sess, _, ok := a.requireSession(c)
	if !ok {
		return
	}
	pwdID, _, stoken := sessn.snapshot()
	files, err := sess.ShareList(c.Request.Context(), pwdID, stoken, c.Query("pdir_fid"))
	if err != nil {
		writeError(c, http.StatusBadGateway, codeShareListFailed, "拉取分享目录失败: "+err.Error())
		return
	}
	// 本页 token 并入缓存（锁内小临界区, 无网络调用）。
	shareSessionsMu.Lock()
	sessn.cacheTokens(files)
	shareSessionsMu.Unlock()

	c.JSON(http.StatusOK, gin.H{"files": files})
}

// handleDriveFiles GET /api/drive/files?pdir_fid=——DriveList 直透。
func (a *App) handleDriveFiles(c *gin.Context) {
	sess, _, ok := a.requireSession(c)
	if !ok {
		return
	}
	files, err := sess.DriveList(c.Request.Context(), c.Query("pdir_fid"))
	if err != nil {
		writeError(c, http.StatusBadGateway, codeDriveListFailed, "拉取网盘目录失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"files": files})
}
