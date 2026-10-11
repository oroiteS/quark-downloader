package protocol

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAPIHeadersComplete(t *testing.T) {
	const cookie = "__puus=token-a; __pus=token-b; ctoken=xyz"

	// UserAgent 必须 §2.2 逐字:夸克网盘 PC 客户端(Electron)身份,关键片段防抄录走样。
	for _, sub := range []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
		"(KHTML, like Gecko) quark-cloud-drive/3.14.2 Chrome/112.0.5615.165",
		"Electron/24.1.3.8 Safari/537.36 Channel/pckk_other_ch",
	} {
		if !strings.Contains(UserAgent, sub) {
			t.Errorf("UserAgent 缺少片段 %q,实际值:%q", sub, UserAgent)
		}
	}

	// 接口域名(drive-pc)→ same-site;完整头模板逐项一致(§2.3)。
	h := APIHeaders(cookie, "https://drive-pc.quark.cn/1/clouddrive/file/sort")
	wantHeaders := [][2]string{
		{"User-Agent", UserAgent},
		{"Accept", "application/json, text/plain, */*"},
		{"Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8"},
		{"Referer", "https://pan.quark.cn/"},
		{"Origin", "https://pan.quark.cn"},
		{"Cache-Control", "no-cache"},
		{"Pragma", "no-cache"},
		{"DNT", "1"},
		{"Connection", "keep-alive"},
		{"Sec-Fetch-Dest", "empty"},
		{"Sec-Fetch-Mode", "cors"},
		{"Sec-Fetch-Site", "same-site"},
		{"Cookie", cookie},
	}
	for _, wh := range wantHeaders {
		if got := h.Get(wh[0]); got != wh[1] {
			t.Errorf("头 %s = %q, want %q", wh[0], got, wh[1])
		}
	}

	// pan 域 URL → same-origin;Cookie 原样带上。
	hPan := APIHeaders(cookie, "https://pan.quark.cn/")
	if got := hPan.Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Errorf("pan 域 URL 的 Sec-Fetch-Site = %q, want %q", got, "same-origin")
	}
	if got := hPan.Get("Cookie"); got != cookie {
		t.Errorf("Cookie = %q, want 原样 %q", got, cookie)
	}
}

func TestCookieJarMergeOrder(t *testing.T) {
	jar := NewCookieJar()

	// §2.3 汇总顺序:pan → drive-pc → drive-m → drive → drive-h → uop → su → quark.cn,
	// 后者覆盖前者。每个域都种入同名 __puus(带 Set-Cookie 属性形态)。
	domains := []string{
		"pan.quark.cn", "drive-pc.quark.cn", "drive-m.quark.cn", "drive.quark.cn",
		"drive-h.quark.cn", "uop.quark.cn", "su.quark.cn", "quark.cn",
	}
	for i, d := range domains {
		jar.Merge(d, []string{
			fmt.Sprintf("__puus=from-%d; Path=/; Domain=.quark.cn; HttpOnly", i),
			fmt.Sprintf("d%d=v%d", i, i),
		})
	}
	final := jar.String()
	if !strings.Contains(final, "__puus=from-7") {
		t.Errorf("同名 Cookie 应由顺序最后的 quark.cn 域胜出,String() = %q", final)
	}
	if strings.Contains(final, "from-0") || strings.Contains(final, "from-3") {
		t.Errorf("被覆盖域的旧值不应残留,String() = %q", final)
	}

	// 覆盖关系由域名优先级表决定,与 Merge 调用先后无关。
	jar2 := NewCookieJar()
	jar2.Merge("quark.cn", []string{"__puus=later-domain"})
	jar2.Merge("pan.quark.cn", []string{"__puus=early-domain"})
	if got := jar2.String(); !strings.Contains(got, "__puus=later-domain") {
		t.Errorf("优先级表靠后的域应始终胜出,String() = %q", got)
	}

	// LoadFromString 导入整串;Has 判存在;导入值优先级最低,可被任意域覆盖。
	jar3 := NewCookieJar()
	jar3.LoadFromString("a=1; b=2")
	if got := jar3.String(); got != "a=1; b=2" {
		t.Errorf("String() = %q, want %q", got, "a=1; b=2")
	}
	if !jar3.Has("a") || !jar3.Has("b") {
		t.Errorf("Has 应能命中已导入的 Cookie,String() = %q", jar3.String())
	}
	if jar3.Has("__puus") {
		t.Error("Has 对不存在的 Cookie 应返回 false")
	}
	jar3.Merge("pan.quark.cn", []string{"a=9"})
	if got := jar3.String(); !strings.Contains(got, "a=9") {
		t.Errorf("Set-Cookie 应覆盖导入串中的同名值,String() = %q", got)
	}
}

func TestThrottlerInterval(t *testing.T) {
	th := NewThrottler()
	ctx := context.Background()

	// 首次调用立即放行,随后相邻两次放行间隔 ∈ [260ms×0.75, 260ms×1.25],
	// 测试边界留时钟容差取 [190ms, 400ms]。
	if err := th.Wait(ctx); err != nil {
		t.Fatalf("首次 Wait: %v", err)
	}
	first := time.Now()
	if err := th.Wait(ctx); err != nil {
		t.Fatalf("第二次 Wait: %v", err)
	}
	elapsed := time.Since(first)
	if elapsed < 190*time.Millisecond {
		t.Errorf("相邻放行间隔 %v 过短,下界 190ms", elapsed)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("相邻放行间隔 %v 过长,上界 400ms", elapsed)
	}

	// ctx 已取消且仍需等待时,应立即返回 ctx.Err() 而非傻等。
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	start := time.Now()
	if err := th.Wait(cancelled); err == nil {
		t.Error("ctx 取消后 Wait 应返回非 nil error")
	} else if time.Since(start) > 100*time.Millisecond {
		t.Errorf("ctx 取消后 Wait 仍耗时 %v 才返回", time.Since(start))
	}
}
