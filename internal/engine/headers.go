// 下载请求头，逐字出自规格 §3.4「下载请求的完整头」。
package engine

import (
	"net/http"

	"quark-downloader/internal/protocol"
)

// DownloadHeaders 返回直链 CDN 下载请求的完整头（§3.4 全套，不含 Range——
// Range 由 worker 按片逐请求追加 bytes=<start>-<end>，end 为闭区间）。
//
// 与 API 头（protocol.APIHeaders）的区别：Accept 为 */*，无 Cache-Control/
// Pragma/DNT/Connection；Sec-Fetch-Site 固定 cross-site（下载直链是跨站请求，
// API 按 URL 判 same-origin/same-site）。
//
// cookieStr 必须是完整 Cookie 串（形如 "a=1; b=2"）：实测不带 Cookie 会被
// CDN 防盗链拒（412）。UserAgent 直接复用 protocol.UserAgent——DlUa=ApiUa
// （§2.2），取链与下载同族一致，否则 CDN 判盗链。
func DownloadHeaders(cookieStr string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", protocol.UserAgent)
	h.Set("Referer", "https://pan.quark.cn/")
	h.Set("Origin", "https://pan.quark.cn")
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "cross-site")
	h.Set("Cookie", cookieStr)
	return h
}
