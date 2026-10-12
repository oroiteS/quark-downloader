// static.go embed 静态资源：GET / 与 SPA fallback。
//
// //go:embed 要求 web/dist 目录在编译期必须存在——本任务提交占位
// index.html 兜底；Task 14 前端构建产物落同名目录后无需改本文件。
package webapi

import (
	"embed"
	"mime"
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

//go:embed web/dist
var distFS embed.FS

// indexPath embed 内 index.html 的全路径。
const indexPath = "web/dist/index.html"

// registerStatic 注册静态路由：GET / 显式命中 index.html；其余未匹配
// 非 /api 路径由 NoRoute（server.go）走 serveStatic 回落。
func registerStatic(r *gin.Engine) {
	r.GET("/", serveIndex)
}

// serveIndex 返回 index.html（SPA 入口）。
func serveIndex(c *gin.Context) {
	b, err := distFS.ReadFile(indexPath)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "internal", "index.html 缺失")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// serveStatic 非 /api 未匹配路径的兜底：先按静态资产找（dist 内真文件
// 原样返回），不存在则回落 index.html——前端 history 路由的 SPA 惯例
// （刷新 /tasks/abc 不应 404，由前端路由接手）。
func serveStatic(c *gin.Context) {
	// path.Clean 归一化并防目录遍历（".." 折叠；embed.FS 本身也不出界，
	// 双保险）；保前导 / 使 Join 后仍是 "web/dist/..." 内的绝对子路径。
	p := path.Clean("/" + c.Request.URL.Path)
	if p == "/" {
		serveIndex(c)
		return
	}
	if b, err := distFS.ReadFile("web/dist" + p); err == nil {
		c.Data(http.StatusOK, contentTypeByExt(p), b)
		return
	}
	serveIndex(c) // SPA fallback
}

// contentTypeByExt 按扩展名给 Content-Type；mime 缺省兜底流式。
func contentTypeByExt(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
