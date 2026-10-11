// exchange.go 实现换 Cookie 协议(规格 §2.5):用扫码拿到的 service_ticket
// 依次访问 pan 域 account/info 与三个网盘 URL,把登录态 Cookie 收进 CookieJar。
// pan 域前置 TLS 指纹墙(§2.1),步骤 1 与步骤 2a 走 Session.wall 过墙客户端;
// drive-pc / drive 域无指纹墙,沿用业务客户端(与 DoAPI 同源)。
package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 步骤 2 两个 API 域 URL 的查询串(§2.5 逐字;路径与 base 分开存放,
// 便于 sessionBase 覆盖域名)。
const (
	fileSortQuery = "pr=ucpro&fr=pc&uc_param_str=" +
		"&pdir_fid=0&_page=1&_size=50&_fetch_total=1&_sort=file_type:asc,updated_at:desc"
	memberQuery = "pr=ucpro&fr=pc&uc_param_str=&fetch_subscribe=true"
	// 步骤 1 特例 Accept(§2.5):其余头仍按 §2.3 模板,仅此项覆盖。
	step1Accept = "text/html,application/xhtml+xml,*/*;q=0.8"
)

// AccountInfo 是换 Cookie 步骤 1 顺带取到的账号信息(§2.5)。
type AccountInfo struct {
	Nickname  string `json:"nickname"`
	AvatarURI string `json:"avatarUri"`
}

// ExchangeCookie 用 service_ticket 换登录 Cookie(§2.5):
//
//	步骤 1  过墙客户端 GET {pan}/account/info?st=<T>&lw=scan,
//	        头按 §2.3 模板 + Accept 覆盖为 text/html 形态;
//	        Set-Cookie 下发 __pus/__kp/__kps/__ktd/__uid/ctoken,
//	        响应 data 顺带携带昵称与头像。
//	步骤 2  依次 GET {pan}/list、{drivePC}/1/clouddrive/file/sort、
//	        {drive}/1/clouddrive/member,补全 __puus 等网盘侧 Cookie;
//	        单个失败记录明细后继续,尽力收全。
//	步骤 3  汇总 jar(整串维护,§2.3)。
//
// 成功判据:jar 含 __pus。失败时错误信息携带各步骤明细供诊断。
// 步骤 2 的 UA 选用 §2.3 模板的客户端 UA(§2.5 允许普通浏览器 UA,
// 保持全套头"完整且前后一致"更不易被识别为异常,见报告说明)。
func (s *Session) ExchangeCookie(ctx context.Context, ticket string) (AccountInfo, error) {
	var info AccountInfo

	// 步骤 1:失败立即返回——拿不到 __pus,成功判据不可能满足。
	step1URL := fmt.Sprintf("%s/account/info?st=%s&lw=scan",
		s.base.pan, url.QueryEscape(ticket))
	data, err := s.exchangeGetJSON(ctx, s.wall, step1URL)
	if err != nil {
		return info, fmt.Errorf("换 Cookie 步骤 1(account/info)失败: %w", err)
	}
	if len(data) > 0 {
		// data 形状不符时昵称/头像留空,不阻断换 Cookie 主流程。
		_ = json.Unmarshal(data, &info)
	}

	// 步骤 2:三个 URL 依次 GET,wall 标记是否走 pan 域过墙客户端。
	steps := []struct {
		name string
		wall bool
		url  string
	}{
		{"pan/list", true, s.base.pan + "/list"},
		{"file/sort", false, s.base.drivePC + "/1/clouddrive/file/sort?" + fileSortQuery},
		{"member", false, s.base.drive + "/1/clouddrive/member?" + memberQuery},
	}
	var step2Errs []string
	for _, st := range steps {
		client := s.client
		if st.wall {
			client = s.wall
		}
		if err := s.exchangeGet(ctx, client, st.url); err != nil {
			step2Errs = append(step2Errs, fmt.Sprintf("%s: %v", st.name, err))
		}
	}

	// 步骤 3 + 成功判据:串中存在 __pus(§2.5)。
	if !s.jar.Has("__pus") {
		err := fmt.Errorf("换 Cookie 失败: Cookie 串缺少 __pus(步骤 1 已请求但服务端未下发登录态)")
		if len(step2Errs) > 0 {
			err = fmt.Errorf("%w;步骤 2 明细: %s", err, strings.Join(step2Errs, "; "))
		}
		return info, err
	}
	return info, nil
}

// exchangeGetJSON 发一个 GET 并按 pan 域外壳判定响应,返回 data 原文:
// 节流 → 完整头模板(Accept 覆盖为 §2.5 步骤 1 的 text/html 形态)→
// 指定客户端发送 → 合并 Set-Cookie → HTTP 200 且外壳成功才算通过。
//
// 注意:pan 域 account/info 的外壳是布尔 success 形态
// {"success":true,"data":{...}}(2026-10-11 真机实测),
// 与业务域的 {status:200,code:0} 外壳(§2.3,adjudicate)不同——
// 若误用 adjudicate 会把合法响应判成"响应不是有效 JSON"导致步骤 1
// 中止、__puus 拿不到。业务域接口仍走 adjudicate,勿动。
func (s *Session) exchangeGetJSON(ctx context.Context, client *http.Client, url string) (json.RawMessage, error) {
	status, raw, err := s.exchangeRequest(ctx, client, url, step1Accept)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", status, url)
	}
	var env struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pan 域外壳判定失败: 响应不是有效 JSON(HTTP %d): %s",
			status, snippet120(raw))
	}
	if !env.Success {
		if env.Message != "" {
			return nil, fmt.Errorf("pan 域外壳 success=false: %s", env.Message)
		}
		return nil, fmt.Errorf("pan 域外壳 success=false: %s", snippet120(raw))
	}
	return env.Data, nil
}

// snippet120 截取响应体前 120 字节作错误摘要,超长以省略号收尾。
func snippet120(raw []byte) string {
	snippet := string(raw)
	if len(snippet) > 120 {
		snippet = snippet[:120] + "..."
	}
	return snippet
}

// exchangeGet 发一个只为收 Set-Cookie 的 GET(§2.5 步骤 2):
// 节流 → 完整头模板 → 指定客户端发送 → 合并 Set-Cookie → 读完 body
// 丢弃(页面/外壳形态不一,不做业务判定,HTTP 200 即通过)。
func (s *Session) exchangeGet(ctx context.Context, client *http.Client, url string) error {
	status, _, err := s.exchangeRequest(ctx, client, url, "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", status, url)
	}
	return nil
}

// exchangeGet/exchangeGetJSON 的共同底层:节流 → 构造请求(完整头模板,
// accept 非空时覆盖 Accept)→ 发送 → 合并 Set-Cookie → 返回状态码与 body。
func (s *Session) exchangeRequest(ctx context.Context, client *http.Client, url, accept string) (int, []byte, error) {
	if err := s.throttle.Wait(ctx); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("构造请求 %s: %w", url, err)
	}
	req.Header = APIHeaders(s.jar.String(), url)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("请求 %s 失败: %w", url, err)
	}
	defer resp.Body.Close()
	// §2.3 规则 2:每次响应的 Set-Cookie 增量合并回整串,先于成功判定。
	s.mergeSetCookies(resp)
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("读取响应 body: %w", err)
	}
	return resp.StatusCode, raw, nil
}
