// cookieimport.go 实现规格 §6.3 Cookie 文本导入的四格式解析：
// JSON 数组（Cookie-Editor 等浏览器扩展导出）/ JSON 对象 / 嵌套 cookie 串
// 字段 / 原始串，统一输出 "k=v; k=v" 整串后灌入 CookieJar。
package webapi

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// parseCookieText 把 §6.3 四格式 Cookie 文本统一解析为 "k=v; k=v" 整串
// （键按名字排序，与 CookieJar.String 的确定性输出一致，断言可直接比对）。
//
//	① JSON 数组 [{"name","value","domain"}...]
//	② JSON 对象 {"a":"1","b":"2"}
//	③ {"cookie": "a=1; b=2"} 嵌套串字段
//	④ 原始串（容许 "Cookie:" 前缀、容许跨行）
//
// 解析不出任何有效条目时返回错误；①过滤后为空也算错误。
func parseCookieText(text string) (string, error) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", fmt.Errorf("Cookie 文本为空")
	}
	switch {
	case strings.HasPrefix(t, "["):
		return parseCookieJSONArray(t)
	case strings.HasPrefix(t, "{"):
		return parseCookieJSONObject(t)
	default:
		return parseCookieRawString(t)
	}
}

// parseCookieJSONArray ① Cookie-Editor 等浏览器扩展导出的条目数组。
//
// ⚠️ §6.3 逐字红线：必须按 domain 过滤只留含 quark 的条目——用户可能
// 导出整个浏览器的 Cookie，几百条无关条目会撑爆请求头（服务端 400）。
// （Review Focus #2：500 条非 quark 混入时 jar 只含 quark 条目，已钉死测试。）
func parseCookieJSONArray(t string) (string, error) {
	var entries []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	}
	if err := json.Unmarshal([]byte(t), &entries); err != nil {
		return "", fmt.Errorf("JSON 数组格式不合法: %v", err)
	}
	kv := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(e.Domain), "quark") {
			continue
		}
		kv[e.Name] = e.Value
	}
	if len(kv) == 0 {
		return "", fmt.Errorf("未找到 quark 域的 Cookie 条目（已按 domain 过滤）")
	}
	return joinCookieKV(kv), nil
}

// parseCookieJSONObject ② 扁平键值对象与 ③ {"cookie":"..."} 嵌套串字段。
// 单键 cookie 且值为字符串 → 按嵌套串处理（内嵌串沿用 ④ 的容错规则）；
// 否则要求全部值为字符串，按 ② 处理。
func parseCookieJSONObject(t string) (string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(t), &obj); err != nil {
		return "", fmt.Errorf("JSON 对象格式不合法: %v", err)
	}
	if raw, ok := obj["cookie"]; ok && len(obj) == 1 {
		var inner string
		if err := json.Unmarshal(raw, &inner); err == nil {
			return parseCookieRawString(inner)
		}
	}
	kv := make(map[string]string, len(obj))
	for k, raw := range obj {
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("JSON 对象的值须为字符串（键 %q）: %v", k, err)
		}
		kv[k] = v
	}
	if len(kv) == 0 {
		return "", fmt.Errorf("JSON 对象为空")
	}
	return joinCookieKV(kv), nil
}

// parseCookieRawString ④ 原始串：剥 "Cookie:" 前缀（大小写不敏感），
// 跨行视为分号等价分隔；无 '=' 或空名的段跳过。
func parseCookieRawString(t string) (string, error) {
	t = strings.TrimSpace(t)
	if len(t) >= 7 && strings.EqualFold(t[:7], "cookie:") {
		t = strings.TrimSpace(t[7:])
	}
	t = strings.NewReplacer("\r", ";", "\n", ";").Replace(t)
	kv := make(map[string]string)
	for _, part := range strings.Split(t, ";") {
		idx := strings.IndexByte(part, '=')
		if idx <= 0 {
			continue
		}
		kv[strings.TrimSpace(part[:idx])] = strings.TrimSpace(part[idx+1:])
	}
	if len(kv) == 0 {
		return "", fmt.Errorf("未从文本中解析出任何 Cookie 键值对")
	}
	return joinCookieKV(kv), nil
}

// joinCookieKV 按键名排序拼成 "k=v; k=v"（与 CookieJar.String 同序）。
func joinCookieKV(kv map[string]string) string {
	names := make([]string, 0, len(kv))
	for n := range kv {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+kv[n])
	}
	return strings.Join(parts, "; ")
}
