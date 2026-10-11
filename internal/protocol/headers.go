// Package protocol 实现飞舞(夸克网盘下载器)与夸克服务端的会话层协议:
// 统一请求头模板、Cookie 整串管理与 API 请求节流。规格见 docs/核心算法原理.md §2.3。
package protocol

import (
	"net/http"
	"strings"
)

// UserAgent 是夸克网盘 PC 客户端(Electron)身份,§2.2 逐字抄录。
// 取直链与实际下载必须同族一致,否则 CDN 判盗链(降速或 412)。
const UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165 " +
	"Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch"

// APIHeaders 返回发往夸克任意 API 的完整请求头模板(§2.3)。
// "完整且前后一致"是设计目标——缺项本身就是异常特征。
// cookieStr 为完整 Cookie 串(形如 "a=1; b=2");url 用于判定 Sec-Fetch-Site:
// URL 含 pan.quark.cn 时为 same-origin,否则 same-site。
func APIHeaders(cookieStr, url string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", UserAgent)
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("Referer", "https://pan.quark.cn/")
	h.Set("Origin", "https://pan.quark.cn")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("DNT", "1")
	h.Set("Connection", "keep-alive")
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	if strings.Contains(url, "pan.quark.cn") {
		h.Set("Sec-Fetch-Site", "same-origin")
	} else {
		h.Set("Sec-Fetch-Site", "same-site")
	}
	h.Set("Cookie", cookieStr)
	return h
}
