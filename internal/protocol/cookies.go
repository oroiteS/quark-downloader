package protocol

import (
	"slices"
	"strings"
	"sync"
)

// domainOrder 是 §2.3 规定的 Cookie 汇总顺序,越靠后优先级越高(后者覆盖前者)。
var domainOrder = []string{
	"pan.quark.cn",
	"drive-pc.quark.cn",
	"drive-m.quark.cn",
	"drive.quark.cn",
	"drive-h.quark.cn",
	"uop.quark.cn",
	"su.quark.cn",
	"quark.cn",
}

// domainRank 返回域名在汇总顺序中的位次;未知域与本地导入同为 -1,
// 优先级低于一切已知域,可被任意服务端 Set-Cookie 覆盖。
func domainRank(domain string) int {
	for i, d := range domainOrder {
		if d == domain {
			return i
		}
	}
	return -1
}

// cookieEntry 记录值与来源域位次,用于按域名优先级裁决同名冲突。
type cookieEntry struct {
	value string
	rank  int
}

// CookieJar 以一整条字符串维护 Cookie(§2.3):不依赖 HTTP 客户端的
// CookieJar 域匹配——接口域名(drive-pc)与登录域(pan)不同,自动匹配不可靠。
type CookieJar struct {
	mu      sync.Mutex
	cookies map[string]cookieEntry
}

// NewCookieJar 创建空 CookieJar。
func NewCookieJar() *CookieJar {
	return &CookieJar{cookies: make(map[string]cookieEntry)}
}

// LoadFromString 导入 "a=1; b=2" 形式的整串 Cookie(如浏览器导出);
// 导入值优先级最低,可被任意后续 Set-Cookie 覆盖。
func (j *CookieJar) LoadFromString(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, part := range strings.Split(s, ";") {
		name, value, ok := splitCookieKV(part)
		if !ok {
			continue
		}
		j.cookies[name] = cookieEntry{value: value, rank: -1}
	}
}

// Merge 解析 domain 下发的若干 Set-Cookie 并增量合并(同名覆盖)。
// 覆盖由域名优先级表裁决:位次靠后的域胜出,与调用先后无关;
// 同域再次下发时取最新值。
func (j *CookieJar) Merge(domain string, setCookies []string) {
	rank := domainRank(domain)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, sc := range setCookies {
		// Set-Cookie 形如 "name=value; Path=/; Domain=.quark.cn",取首段键值对。
		if idx := strings.IndexByte(sc, ';'); idx >= 0 {
			sc = sc[:idx]
		}
		name, value, ok := splitCookieKV(sc)
		if !ok {
			continue
		}
		if old, exists := j.cookies[name]; !exists || rank >= old.rank {
			j.cookies[name] = cookieEntry{value: value, rank: rank}
		}
	}
}

// String 输出 "k=v; k=v" 形式的完整 Cookie 串,按名字排序保证确定性。
func (j *CookieJar) String() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	names := make([]string, 0, len(j.cookies))
	for name := range j.cookies {
		names = append(names, name)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+j.cookies[name].value)
	}
	return strings.Join(parts, "; ")
}

// Has 报告 Cookie 存在且值非空。空值(登出类 Set-Cookie 置空)不算存在,
// 以支撑 §2.2 "__puus 是登录成功唯一可靠判据"的用法。
func (j *CookieJar) Has(name string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.cookies[name]
	return ok && e.value != ""
}

// splitCookieKV 把 "name=value" 切成键值;无 '=' 或名字为空时 ok=false。
func splitCookieKV(s string) (name, value string, ok bool) {
	s = strings.TrimSpace(s)
	idx := strings.IndexByte(s, '=')
	if idx <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:]), true
}
