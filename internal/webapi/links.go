// links.go 直链获取与导出端点（Task 13）：
//
//	POST /api/links ← {source: share|drive, fids[], sid?, tokens?} ——
//	  share 模式 sid 引用 parse 会话补 pwd_id/stoken（免转存三参的
//	  凭证半），tokens 可选对位传 share_fid_token（缺省从会话 token
//	  缓存逐 fid 补——parse/files 导航时并入）；drive 模式 body 只
//	  fids。DownloadLinks 打分结果透出 items + expires_hint（直链有
//	  实效, 提示尽快使用）。
//	POST /api/links/export ← {items:[{rel_path,url}], format} ——
//	  ExportLinks 五格式文本（防盗链模板内嵌 cookie/referer/ua, 故
//	  须登录态）。
package webapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"quark-downloader/internal/protocol"
)

// linksExpiresHint 直链时效提示（brief：响应带 expires_hint）。
const linksExpiresHint = "直链有时效，请尽快使用或在失效后重新获取"

// linksRequest POST /api/links 请求体（tokens 为 share 模式的
// share_fid_token 对位数组, 字段名细化于 §6.2 草案）。
type linksRequest struct {
	Source string   `json:"source"`
	FIDs   []string `json:"fids"`
	SID    string   `json:"sid"`
	Tokens []string `json:"tokens"`
}

// handleLinks POST /api/links——见文件头注释。
func (a *App) handleLinks(c *gin.Context) {
	var req linksRequest
	if err := c.ShouldBindJSON(&req); err != nil ||
		(req.Source != "share" && req.Source != "drive") || len(req.FIDs) == 0 {
		writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "请求须为 {source: share|drive, fids[] 非空}")
		return
	}

	sess, _, ok := a.requireSession(c)
	if !ok {
		return
	}

	lr := protocol.LinkRequest{FIDs: req.FIDs}
	if req.Source == "share" {
		if req.SID == "" {
			writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "share 模式须带 sid（先走 /api/share/parse）")
			return
		}
		shareSessionsMu.Lock()
		s := shareSessions[req.SID]
		shareSessionsMu.Unlock()
		if s == nil {
			writeError(c, http.StatusNotFound, codeNoShareSession, "分享会话不存在或已失效, 请重新解析")
			return
		}
		pwdID, _, stoken := s.snapshot()
		lr.PwdID, lr.Stoken = pwdID, stoken
		// tokens：显式对位优先, 缺省从会话缓存补（miss 留空——协议层
		// 三入口择优自行处理）。
		if len(req.Tokens) > 0 {
			if len(req.Tokens) != len(req.FIDs) {
				writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "tokens 与 fids 须对位等长")
				return
			}
			lr.FIDsToken = req.Tokens
		} else {
			shareSessionsMu.Lock()
			toks := make([]string, len(req.FIDs))
			for i, fid := range req.FIDs {
				toks[i] = s.tokenCache[fid]
			}
			shareSessionsMu.Unlock()
			lr.FIDsToken = toks
		}
	}

	results, err := sess.DownloadLinks(c.Request.Context(), lr)
	if err != nil {
		writeError(c, http.StatusBadGateway, codeLinksFailed, "批量取直链失败: "+err.Error())
		return
	}

	items := make([]gin.H, len(results))
	for i, r := range results {
		items[i] = gin.H{
			"file_name": r.FileName,
			"size":      r.Size,
			"url":       r.URL,
			"score":     r.Score,
		}
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "expires_hint": linksExpiresHint})
}

// linksExportItem 导出请求的单条（与 protocol.ExportItem 对应）。
type linksExportItem struct {
	RelPath string `json:"rel_path"`
	URL     string `json:"url"`
}

// linksExportRequest POST /api/links/export 请求体。
type linksExportRequest struct {
	Items  []linksExportItem `json:"items"`
	Format string            `json:"format"`
}

// handleLinksExport POST /api/links/export——五格式文本导出。
func (a *App) handleLinksExport(c *gin.Context) {
	var req linksExportRequest
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Items) == 0 {
		writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "items 为空或请求非法")
		return
	}
	switch protocol.ExportFormat(req.Format) {
	case protocol.ExportPlain, protocol.ExportNamed, protocol.ExportAria2,
		protocol.ExportCurl, protocol.ExportMotrix:
	default:
		writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "未知导出格式: "+req.Format)
		return
	}

	_, cookieStr, ok := a.requireSession(c)
	if !ok {
		return
	}
	items := make([]protocol.ExportItem, len(req.Items))
	for i, it := range req.Items {
		items[i] = protocol.ExportItem{RelPath: it.RelPath, URL: it.URL}
	}
	text, err := protocol.ExportLinks(items, cookieStr, protocol.ExportFormat(req.Format))
	if err != nil {
		writeError(c, http.StatusBadRequest, codeInvalidLinksReq, "导出失败: "+err.Error())
		return
	}
	c.String(http.StatusOK, text)
}
