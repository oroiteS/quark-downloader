package engine

import (
	"testing"

	"quark-downloader/internal/protocol"
)

// §3.4 下载头全套逐项断言（Range 除外——由 worker 按片设置，此处不得预置）。
func TestDownloadHeaders(t *testing.T) {
	const cookie = "__pus=uid123; _puus=token456; __uid=789"
	h := DownloadHeaders(cookie)

	want := map[string]string{
		"User-Agent":      protocol.UserAgent, // DlUa=ApiUa（§2.2），与取链同族
		"Referer":         "https://pan.quark.cn/",
		"Origin":          "https://pan.quark.cn",
		"Accept":          "*/*",
		"Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
		"Sec-Fetch-Dest":  "empty",
		"Sec-Fetch-Mode":  "cors",
		"Sec-Fetch-Site":  "cross-site", // 与 API 头不同：下载直链是跨站请求
		"Cookie":          cookie,       // 原样整串；实测缺失会被 CDN 防盗链拒（412）
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, 期望 %q", k, got, v)
		}
	}
	if _, ok := h["Range"]; ok {
		t.Error("DownloadHeaders 不应预置 Range（由 worker 按片追加 bytes=<start>-<end>）")
	}
	if len(h) != len(want) {
		t.Errorf("共 %d 个头, 期望恰好 %d 个（不得夹带规格未列出的项）", len(h), len(want))
	}
}
