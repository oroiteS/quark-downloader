package webapi

import (
	"fmt"
	"strings"
	"testing"
)

// TestParseCookieTextFormats §6.3 四格式各一例：统一输出 "k=v; k=v"
// （按键名排序，与 CookieJar.String 的确定性输出一致）。
func TestParseCookieTextFormats(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "① JSON 数组(Cookie-Editor 导出)",
			text: `[{"name":"__puus","value":"abc","domain":".quark.cn"},
			       {"name":"_UP_A4","value":"x","domain":".quark.cn"},
			       {"name":"baidu_key","value":"1","domain":".baidu.com"},
			       {"name":"taobao_key","value":"2","domain":"www.taobao.com"}]`,
			want: "_UP_A4=x; __puus=abc",
		},
		{
			name: "② JSON 对象",
			text: `{"__puus":"u2","a":"1"}`,
			want: "__puus=u2; a=1",
		},
		{
			name: "③ 嵌套 cookie 串字段",
			text: `{"cookie":"__puus=u3; b=2"}`,
			want: "__puus=u3; b=2",
		},
		{
			name: "④ 原始串: Cookie 前缀 + 跨行",
			text: "Cookie: __puus=u4;\r\nc=3\n",
			want: "__puus=u4; c=3",
		},
		{
			name: "④ 原始串: 裸串无前缀",
			text: "__puus=u5; __pus=p",
			want: "__pus=p; __puus=u5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCookieText(tc.text)
			if err != nil {
				t.Fatalf("parseCookieText(%q) 报错: %v", tc.text, err)
			}
			if got != tc.want {
				t.Fatalf("parseCookieText = %q, 期望 %q", got, tc.want)
			}
		})
	}
}

// TestParseCookieTextFiltersNonQuark500 Review Focus #2 红线（逐字钉死）：
// 混入 500 条非 quark 条目（模拟用户导出整个浏览器 Cookie）时，
// ①必须按 domain 过滤只留含 quark 的条目——几百条无关条目会撑爆请求头。
func TestParseCookieTextFiltersNonQuark500(t *testing.T) {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < 500; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"junk%d","value":"x","domain":".example%d.com"}`, i, i%7)
	}
	b.WriteString(`,{"name":"__puus","value":"tok","domain":".quark.cn"}`)
	b.WriteString(`,{"name":"__pus","value":"p","domain":"pan.quark.cn"}`)
	b.WriteByte(']')

	got, err := parseCookieText(b.String())
	if err != nil {
		t.Fatalf("parseCookieText 报错: %v", err)
	}
	if want := "__pus=p; __puus=tok"; got != want {
		t.Fatalf("过滤后 Cookie = %q, 期望 %q", got, want)
	}
	if strings.Contains(got, "junk") {
		t.Fatalf("非 quark 条目泄漏进结果: %q", got)
	}
}

// TestParseCookieTextErrors 解析失败各情形：一律报错而非静默空串。
func TestParseCookieTextErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // 错误信息应包含的片段
	}{
		{"空串", "", "为空"},
		{"纯空白", "   \n\t ", "为空"},
		{"坏 JSON 数组", `[{"name":}]`, "JSON"},
		{"全非 quark 数组", `[{"name":"a","value":"1","domain":".baidu.com"}]`, "quark"},
		{"JSON 对象值非字符串", `{"a":{"b":"1"}}`, "字符串"},
		{"空 JSON 对象", `{}`, "为空"},
		{"垃圾原始串", "没有等号的文本", "键值对"},
		{"嵌套串为空", `{"cookie":""}`, "键值对"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCookieText(tc.text)
			if err == nil {
				t.Fatalf("parseCookieText(%q) = %q, 期望报错", tc.text, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}
